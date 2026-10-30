// Package procmon maps network sockets to processes via /proc.
//
// It parses /proc/net/{tcp,tcp6,udp,udp6} to learn socket tuples and their
// inode, then scans /proc/<pid>/fd to find which process owns each inode.
package procmon

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Key is a full socket tuple.
type Key struct {
	Proto      uint8 // 6 = TCP, 17 = UDP
	LocalIP    netip.Addr
	LocalPort  uint16
	RemoteIP   netip.Addr
	RemotePort uint16
}

// LocalKey matches a socket bound to a specific local address.
type LocalKey struct {
	Proto     uint8
	LocalIP   netip.Addr
	LocalPort uint16
}

// PortKey matches a socket by protocol and local port only
// (covers sockets bound to 0.0.0.0 / ::).
type PortKey struct {
	Proto uint8
	Port  uint16
}

// ProcInfo describes a process.
type ProcInfo struct {
	PID  int32
	Name string // executable basename (comm fallback)
	Exe  string
}

// Table is an immutable snapshot of socket→process mappings.
type Table struct {
	exact   map[Key]int32
	byLocal map[LocalKey]int32
	byPort  map[PortKey]int32
	Procs   map[int32]ProcInfo
}

// Resolve finds the PID owning the socket identified by the tuple.
// It tries exact tuple, then local addr:port, then proto:port.
func (t *Table) Resolve(proto uint8, localIP netip.Addr, localPort uint16, remoteIP netip.Addr, remotePort uint16) (int32, bool) {
	if t == nil {
		return 0, false
	}
	if pid, ok := t.exact[Key{proto, localIP, localPort, remoteIP, remotePort}]; ok {
		return pid, true
	}
	if pid, ok := t.byLocal[LocalKey{proto, localIP, localPort}]; ok {
		return pid, true
	}
	if pid, ok := t.byPort[PortKey{proto, localPort}]; ok {
		return pid, true
	}
	return 0, false
}

// Proc returns info for a pid (or a stub if unknown).
func (t *Table) Proc(pid int32) ProcInfo {
	if t == nil {
		return ProcInfo{PID: pid, Name: "?"}
	}
	if p, ok := t.Procs[pid]; ok {
		return p
	}
	return ProcInfo{PID: pid, Name: "?"}
}

type sockEntry struct {
	key   Key
	inode uint64
}

// Scan builds a fresh Table from /proc.
func Scan() (*Table, error) {
	var socks []sockEntry
	for _, f := range []struct {
		path  string
		proto uint8
		v6    bool
	}{
		{"/proc/net/tcp", 6, false},
		{"/proc/net/tcp6", 6, true},
		{"/proc/net/udp", 17, false},
		{"/proc/net/udp6", 17, true},
	} {
		entries, err := parseNetFile(f.path, f.proto, f.v6)
		if err != nil {
			continue // file may not exist (e.g. ipv6 disabled)
		}
		socks = append(socks, entries...)
	}

	want := make(map[uint64]struct{}, len(socks))
	for _, s := range socks {
		if s.inode != 0 {
			want[s.inode] = struct{}{}
		}
	}

	inodePID, procs := scanFds(want)

	t := &Table{
		exact:   make(map[Key]int32, len(socks)),
		byLocal: make(map[LocalKey]int32, len(socks)),
		byPort:  make(map[PortKey]int32, len(socks)),
		Procs:   procs,
	}
	for _, s := range socks {
		pid, ok := inodePID[s.inode]
		if !ok {
			continue
		}
		k := s.key
		t.exact[k] = pid
		lk := LocalKey{k.Proto, k.LocalIP, k.LocalPort}
		if _, exists := t.byLocal[lk]; !exists {
			t.byLocal[lk] = pid
		}
		pk := PortKey{k.Proto, k.LocalPort}
		if _, exists := t.byPort[pk]; !exists {
			t.byPort[pk] = pid
		}
	}
	return t, nil
}

// parseNetFile parses /proc/net/{tcp,tcp6,udp,udp6}.
func parseNetFile(path string, proto uint8, v6 bool) ([]sockEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	var out []sockEntry
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		lip, lport, err := parseAddrPort(fields[1], v6)
		if err != nil {
			continue
		}
		rip, rport, err := parseAddrPort(fields[2], v6)
		if err != nil {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, sockEntry{
			key:   Key{proto, lip, lport, rip, rport},
			inode: inode,
		})
	}
	return out, nil
}

// parseAddrPort parses "0100007F:1F90" (hex, little-endian words).
func parseAddrPort(s string, v6 bool) (netip.Addr, uint16, error) {
	host, portStr, ok := strings.Cut(s, ":")
	if !ok {
		return netip.Addr{}, 0, fmt.Errorf("bad addrport %q", s)
	}
	port64, err := strconv.ParseUint(portStr, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, err
	}
	want := 8
	if v6 {
		want = 32
	}
	if len(host) != want {
		return netip.Addr{}, 0, fmt.Errorf("bad hex addr %q", host)
	}
	if !v6 {
		v, err := strconv.ParseUint(host, 16, 32)
		if err != nil {
			return netip.Addr{}, 0, err
		}
		b := [4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
		return netip.AddrFrom4(b), uint16(port64), nil
	}
	var b [16]byte
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseUint(host[i*8:(i+1)*8], 16, 32)
		if err != nil {
			return netip.Addr{}, 0, err
		}
		b[i*4+0] = byte(v)
		b[i*4+1] = byte(v >> 8)
		b[i*4+2] = byte(v >> 16)
		b[i*4+3] = byte(v >> 24)
	}
	return netip.AddrFrom16(b).Unmap(), uint16(port64), nil
}

// scanFds walks /proc/<pid>/fd looking for socket:[inode] links whose inode
// is in want. It also collects process names.
func scanFds(want map[uint64]struct{}) (map[uint64]int32, map[int32]ProcInfo) {
	inodePID := make(map[uint64]int32, len(want))
	procs := make(map[int32]ProcInfo)

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return inodePID, procs
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid64, err := strconv.ParseInt(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		pid := int32(pid64)
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // process exited or no permission
		}
		matched := false
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(link[8:len(link)-1], 10, 64)
			if err != nil {
				continue
			}
			if _, ok := want[inode]; ok {
				inodePID[inode] = pid
				matched = true
			}
		}
		if matched {
			procs[pid] = readProcInfo(pid)
		}
	}
	return inodePID, procs
}

// readProcInfo reads the process name, preferring the exe basename.
func readProcInfo(pid int32) ProcInfo {
	p := ProcInfo{PID: pid, Name: "?"}
	base := fmt.Sprintf("/proc/%d", pid)
	if exe, err := os.Readlink(base + "/exe"); err == nil && exe != "" {
		p.Exe = exe
		p.Name = filepath.Base(exe)
		return p
	}
	if comm, err := os.ReadFile(base + "/comm"); err == nil {
		p.Name = strings.TrimSpace(string(comm))
	}
	return p
}
