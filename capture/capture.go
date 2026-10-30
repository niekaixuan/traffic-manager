// Package capture sniffs packets with AF_PACKET and attributes them to sockets.
package capture

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/afpacket"
	"github.com/google/gopacket/layers"

	"traffic-manager/procmon"
	"traffic-manager/stats"
)

// Sniffer captures packets on one interface and feeds the stats store.
type Sniffer struct {
	Iface string
	Store *stats.Store

	mu       sync.RWMutex
	table    *procmon.Table
	localNets []netip.Prefix
	localIPs  map[netip.Addr]struct{}

	// unresolved tuple -> last resolve attempt, for retry
	pendingMu sync.Mutex
	pending   map[unresolvedKey]time.Time

	rescanEvery time.Duration

	packets uint64
	dropped uint64
}

type unresolvedKey struct {
	proto                    uint8
	localIP, remoteIP        netip.Addr
	localPort, remotePort    uint16
}

func NewSniffer(iface string, store *stats.Store) *Sniffer {
	return &Sniffer{
		Iface:       iface,
		Store:       store,
		localIPs:    make(map[netip.Addr]struct{}),
		pending:     make(map[unresolvedKey]time.Time),
		rescanEvery: 2 * time.Second,
	}
}

// RefreshLocalAddrs rebuilds the set of local IPs and prefixes.
func (s *Sniffer) RefreshLocalAddrs() error {
	iface, err := net.InterfaceByName(s.Iface)
	if err != nil {
		return err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}
	ips := make(map[netip.Addr]struct{})
	var nets []netip.Prefix
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		ips[ip] = struct{}{}
		ones, _ := ipnet.Mask.Size()
		if ip.Is4() {
			nets = append(nets, netip.PrefixFrom(ip, ones))
		}
	}
	s.mu.Lock()
	s.localIPs = ips
	s.localNets = nets
	s.mu.Unlock()
	return nil
}

// UpdateTable swaps the socket->pid table.
func (s *Sniffer) UpdateTable(t *procmon.Table) {
	s.mu.Lock()
	s.table = t
	s.mu.Unlock()
}

// Stats returns packet/drop counters.
func (s *Sniffer) Stats() (packets, dropped uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.packets, s.dropped
}

// Run starts capturing until ctx is cancelled.
func (s *Sniffer) Run(ctx context.Context) error {
	if err := s.RefreshLocalAddrs(); err != nil {
		return fmt.Errorf("iface %s: %w", s.Iface, err)
	}
	h, err := afpacket.NewTPacket(
		afpacket.OptInterface(s.Iface),
		afpacket.OptPollTimeout(200*time.Millisecond),
		afpacket.OptFrameSize(4096),
		afpacket.OptBlockSize(1<<20),
		afpacket.OptNumBlocks(64),
	)
	if err != nil {
		return fmt.Errorf("afpacket on %s: %w (need root or CAP_NET_RAW)", s.Iface, err)
	}
	defer h.Close()

	src := gopacket.NewPacketSource(h, layers.LinkTypeEthernet)
	src.Lazy = true
	src.NoCopy = true
	src.DecodeOptions = gopacket.DecodeOptions{Lazy: true, NoCopy: true}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pkt, err := src.NextPacket()
		if err != nil {
			if errors.Is(err, afpacket.ErrTimeout) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		s.handle(pkt)
	}
}

// handle decodes one packet and records it.
func (s *Sniffer) handle(pkt gopacket.Packet) {
	s.mu.Lock()
	s.packets++
	s.mu.Unlock()

	var (
		srcIP, dstIP netip.Addr
		proto        uint8
		srcPort      uint16
		dstPort      uint16
		length       uint64
	)

	switch net := pkt.NetworkLayer().(type) {
	case *layers.IPv4:
		srcIP = netip.AddrFrom4([4]byte(net.SrcIP.To4()))
		dstIP = netip.AddrFrom4([4]byte(net.DstIP.To4()))
		proto = uint8(net.Protocol)
		length = uint64(net.Length)
	case *layers.IPv6:
		srcIP = netip.AddrFrom16([16]byte(net.SrcIP))
		dstIP = netip.AddrFrom16([16]byte(net.DstIP))
		proto = uint8(net.NextHeader)
		length = uint64(net.Length) + 40
	default:
		return
	}

	if proto != 6 && proto != 17 {
		return
	}

	switch tr := pkt.TransportLayer().(type) {
	case *layers.TCP:
		srcPort = uint16(tr.SrcPort)
		dstPort = uint16(tr.DstPort)
	case *layers.UDP:
		srcPort = uint16(tr.SrcPort)
		dstPort = uint16(tr.DstPort)
	default:
		return
	}

	now := time.Now()
	s.mu.RLock()
	table := s.table
	_, srcLocal := s.localIPs[srcIP]
	_, dstLocal := s.localIPs[dstIP]
	s.mu.RUnlock()

	// Determine direction. Outbound: src is local. Inbound: dst is local.
	switch {
	case srcLocal && !dstLocal:
		s.record(table, srcIP, srcPort, dstIP, dstPort, proto, length, false, now)
	case dstLocal && !srcLocal:
		s.record(table, dstIP, dstPort, srcIP, srcPort, proto, length, true, now)
	case srcLocal && dstLocal:
		// loopback: count once as rx for dst side
		s.record(table, dstIP, dstPort, srcIP, srcPort, proto, length, true, now)
	default:
		// neither endpoint is ours: forwarded traffic, ignore
	}
}

// record attributes a packet to a process.
func (s *Sniffer) record(table *procmon.Table, localIP netip.Addr, localPort uint16, remoteIP netip.Addr, remotePort uint16, proto uint8, n uint64, rx bool, now time.Time) {
	pid, ok := table.Resolve(proto, localIP, localPort, remoteIP, remotePort)
	if !ok {
		pid, ok = s.resolveRetry(localIP, localPort, remoteIP, remotePort, proto, now)
	}
	if !ok {
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
		return
	}
	p := table.Proc(pid)
	s.Store.Record(pid, p.Name, p.Exe, remoteIP, remotePort, proto, n, rx, now)
}

// resolveRetry re-scans /proc at most once per rescanEvery for unknown tuples.
func (s *Sniffer) resolveRetry(localIP netip.Addr, localPort uint16, remoteIP netip.Addr, remotePort uint16, proto uint8, now time.Time) (int32, bool) {
	key := unresolvedKey{proto, localIP, remoteIP, localPort, remotePort}

	s.pendingMu.Lock()
	last, seen := s.pending[key]
	if seen && now.Sub(last) < s.rescanEvery {
		s.pendingMu.Unlock()
		return 0, false
	}
	s.pending[key] = now
	if len(s.pending) > 4096 { // bound memory
		for k := range s.pending {
			delete(s.pending, k)
			break
		}
	}
	s.pendingMu.Unlock()

	t, err := procmon.Scan()
	if err != nil {
		return 0, false
	}
	s.UpdateTable(t)
	pid, ok := t.Resolve(proto, localIP, localPort, remoteIP, remotePort)
	return pid, ok
}
