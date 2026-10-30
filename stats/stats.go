// Package stats aggregates per-process traffic counters and computes rates.
package stats

import (
	"net/netip"
	"sync"
	"time"
)

// FlowKey identifies one direction of traffic for a process on a remote endpoint.
type FlowKey struct {
	PID        int32
	RemoteIP   netip.Addr
	RemotePort uint16
	Proto      uint8
}

// Flow counters for a single (pid, remote, proto) pair.
type Flow struct {
	RxBytes  uint64
	TxBytes  uint64
	RxPkts   uint64
	TxPkts   uint64
	LastSeen time.Time
}

// App is aggregated traffic for one process.
type App struct {
	PID      int32
	Name     string
	Exe      string
	RxBytes  uint64
	TxBytes  uint64
	RxPkts   uint64
	TxPkts   uint64
	RxRate   float64 // bytes/sec, EWMA
	TxRate   float64
	Flows    int
	LastSeen time.Time

	prevRx uint64
	prevTx uint64
}

// Store accumulates counters. Safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	apps   map[int32]*App
	flows  map[FlowKey]*Flow
	names  map[int32]string // pid -> name cache (survives process exit)
	byApp  map[string]*AppTotals // cumulative per app name, never pruned
	now    time.Time
	alpha  float64 // EWMA smoothing factor
	Total  App     // unnamed: global totals
}

// AppTotals is a cumulative counter for one app name.
type AppTotals struct {
	Name    string
	RxBytes uint64
	TxBytes uint64
}

func New() *Store {
	return &Store{
		apps:  make(map[int32]*App),
		flows: make(map[FlowKey]*Flow),
		names: make(map[int32]string),
		byApp: make(map[string]*AppTotals),
		alpha: 0.4,
	}
}

// ByAppCumulative returns cumulative counters keyed by app name.
// Used by the daemon to compute deltas for persistence.
func (s *Store) ByAppCumulative() []AppTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AppTotals, 0, len(s.byApp))
	for _, t := range s.byApp {
		out = append(out, *t)
	}
	return out
}

// Record adds bytes for pid. dirRx = packet received by local process.
func (s *Store) Record(pid int32, name, exe string, remoteIP netip.Addr, remotePort uint16, proto uint8, n uint64, dirRx bool, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if pid >= 0 && name != "" {
		s.names[pid] = name
	}
	a := s.appLocked(pid, name, exe)
	fk := FlowKey{pid, remoteIP, remotePort, proto}
	f := s.flows[fk]
	if f == nil {
		f = &Flow{}
		s.flows[fk] = f
	}
	f.LastSeen = t
	a.LastSeen = t

	if dirRx {
		a.RxBytes += n
		a.RxPkts++
		f.RxBytes += n
		f.RxPkts++
		s.Total.RxBytes += n
		s.Total.RxPkts++
	} else {
		a.TxBytes += n
		a.TxPkts++
		f.TxBytes += n
		f.TxPkts++
		s.Total.TxBytes += n
		s.Total.TxPkts++
	}
	s.Total.LastSeen = t

	appName := name
	if appName == "" {
		appName = "?"
	}
	bt := s.byApp[appName]
	if bt == nil {
		bt = &AppTotals{Name: appName}
		s.byApp[appName] = bt
	}
	if dirRx {
		bt.RxBytes += n
	} else {
		bt.TxBytes += n
	}
}

func (s *Store) appLocked(pid int32, name, exe string) *App {
	a := s.apps[pid]
	if a == nil {
		a = &App{PID: pid, Name: name, Exe: exe}
		s.apps[pid] = a
	}
	if name != "" {
		a.Name = name
	}
	if exe != "" {
		a.Exe = exe
	}
	return a
}

// Tick recomputes EWMA rates. Call once per interval (e.g. 1s).
// It uses deltas since the previous tick.
func (s *Store) Tick(t time.Time, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dt := interval.Seconds()
	if dt <= 0 {
		return
	}
	s.now = t

	// store previous byte counters to derive deltas
	for _, a := range s.apps {
		rxDelta := float64(a.RxBytes - a.prevRx)
		txDelta := float64(a.TxBytes - a.prevTx)
		a.prevRx = a.RxBytes
		a.prevTx = a.TxBytes
		instantRx := rxDelta / dt
		instantTx := txDelta / dt
		a.RxRate = s.alpha*instantRx + (1-s.alpha)*a.RxRate
		a.TxRate = s.alpha*instantTx + (1-s.alpha)*a.TxRate
		a.Flows = 0
	}
	for fk, f := range s.flows {
		if a := s.apps[fk.PID]; a != nil {
			a.Flows++
		}
		// prune flows idle > 5 min
		if t.Sub(f.LastSeen) > 5*time.Minute {
			delete(s.flows, fk)
		}
	}
	// prune processes idle > 5 min with zero rate
	for pid, a := range s.apps {
		if t.Sub(a.LastSeen) > 5*time.Minute {
			delete(s.apps, pid)
			delete(s.names, pid)
		}
	}

	rxDelta := float64(s.Total.RxBytes - s.Total.prevRx)
	txDelta := float64(s.Total.TxBytes - s.Total.prevTx)
	s.Total.prevRx = s.Total.RxBytes
	s.Total.prevTx = s.Total.TxBytes
	s.Total.RxRate = s.alpha*rxDelta/dt + (1-s.alpha)*s.Total.RxRate
	s.Total.TxRate = s.alpha*txDelta/dt + (1-s.alpha)*s.Total.TxRate
}

// Snapshot returns a copy of all apps sorted by total bytes desc.
func (s *Store) Snapshot() []App {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]App, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, *a)
	}
	return out
}

// TotalSnapshot returns global totals.
func (s *Store) TotalSnapshot() App {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.Total
	t.Flows = len(s.flows)
	return t
}

// FlowsFor returns flows of a pid, for the detail view.
func (s *Store) FlowsFor(pid int32) []FlowDetail {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FlowDetail
	for fk, f := range s.flows {
		if fk.PID != pid {
			continue
		}
		out = append(out, FlowDetail{
			RemoteIP:   fk.RemoteIP,
			RemotePort: fk.RemotePort,
			Proto:      fk.Proto,
			RxBytes:    f.RxBytes,
			TxBytes:    f.TxBytes,
		})
	}
	return out
}

// FlowDetail is a read-only flow view.
type FlowDetail struct {
	RemoteIP   netip.Addr
	RemotePort uint16
	Proto      uint8
	RxBytes    uint64
	TxBytes    uint64
}
