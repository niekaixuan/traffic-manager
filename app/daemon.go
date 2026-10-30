package app

import (
	"context"
	"log"
	"time"

	"traffic-manager/storage"
)

const (
	// FlushInterval is how often the daemon writes counters to disk.
	FlushInterval = 30 * time.Second
	// HourlyKeep is the retention for hour-granularity rows.
	HourlyKeep = 8 * 24 * time.Hour
	// DailyKeep is the retention for day-granularity rows.
	DailyKeep = 400 * 24 * time.Hour
)

// RunDaemon captures traffic and periodically persists it. Blocks until ctx is done.
func RunDaemon(ctx context.Context, mon *Monitor, db *storage.DB) error {
	if err := mon.Start(ctx); err != nil {
		return err
	}
	defer mon.Stop()

	last := map[string]storage.Delta{} // app -> last flushed cumulative
	flush := func() {
		now := time.Now()
		var deltas []storage.Delta
		for _, t := range mon.Store.ByAppCumulative() {
			prev := last[t.Name]
			drx := t.RxBytes - prev.Rx
			dtx := t.TxBytes - prev.Tx
			if t.RxBytes < prev.Rx { // counter reset (daemon restart handled by fresh map)
				drx = t.RxBytes
			}
			if t.TxBytes < prev.Tx {
				dtx = t.TxBytes
			}
			if drx > 0 || dtx > 0 {
				deltas = append(deltas, storage.Delta{App: t.Name, Rx: drx, Tx: dtx})
			}
			last[t.Name] = storage.Delta{App: t.Name, Rx: t.RxBytes, Tx: t.TxBytes}
		}
		if err := db.Flush(now, deltas); err != nil {
			log.Printf("flush: %v", err)
		}
		if err := db.Prune(now, HourlyKeep, DailyKeep); err != nil {
			log.Printf("prune: %v", err)
		}
	}

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	log.Printf("daemon started, flushing every %s", FlushInterval)
	for {
		select {
		case <-ctx.Done():
			flush() // final flush on shutdown
			return ctx.Err()
		case <-ticker.C:
			flush()
		}
	}
}
