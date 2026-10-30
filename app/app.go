// Package app wires capture, procmon and stats together.
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"traffic-manager/capture"
	"traffic-manager/procmon"
	"traffic-manager/stats"
)

// Monitor manages sniffers on multiple interfaces.
type Monitor struct {
	Store    *stats.Store
	sniffers []*capture.Sniffer
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewMonitor creates sniffers for the given interfaces.
func NewMonitor(store *stats.Store, ifaces []string) (*Monitor, error) {
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("no interfaces")
	}
	m := &Monitor{Store: store}
	for _, iface := range ifaces {
		m.sniffers = append(m.sniffers, capture.NewSniffer(iface, store))
	}
	return m, nil
}

// Start launches capture goroutines and the periodic table refresher.
func (m *Monitor) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	// initial table
	t, err := procmon.Scan()
	if err != nil {
		return fmt.Errorf("proc scan: %w", err)
	}
	for _, s := range m.sniffers {
		s.UpdateTable(t)
	}

	for _, s := range m.sniffers {
		s := s
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			_ = s.Run(ctx)
		}()
	}

	// periodic rescan: pick up new sockets, refresh local addrs
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t, err := procmon.Scan()
				if err == nil {
					for _, s := range m.sniffers {
						s.UpdateTable(t)
					}
				}
				for _, s := range m.sniffers {
					_ = s.RefreshLocalAddrs()
				}
			}
		}
	}()

	// rate ticker
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				m.Store.Tick(now, now.Sub(last))
				last = now
			}
		}
	}()

	return nil
}

// Stop shuts everything down.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// CaptureStats aggregates packet counters across interfaces.
func (m *Monitor) CaptureStats() (packets, dropped uint64) {
	for _, s := range m.sniffers {
		p, d := s.Stats()
		packets += p
		dropped += d
	}
	return
}
