// Package storage persists per-app traffic to SQLite with hourly and daily granularity.
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite handle.
type DB struct {
	db *sql.DB
}

// DefaultPath returns the default database location.
func DefaultPath() string {
	if os.Geteuid() == 0 {
		return "/var/lib/traffic-manager/traffic.db"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "traffic-manager", "traffic.db")
}

// Open opens (or creates) the database.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite handles one writer at a time; a single connection avoids SQLITE_BUSY
	// between our own goroutines. Readers (TUI) use a separate process.
	db.SetMaxOpenConns(1)
	d := &DB{db: db}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) migrate() error {
	_, err := d.db.Exec(`
CREATE TABLE IF NOT EXISTS hourly (
    hour TEXT NOT NULL,      -- "2006-01-02T15" local hour bucket
    app  TEXT NOT NULL,
    rx   INTEGER NOT NULL DEFAULT 0,
    tx   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, app)
);
CREATE TABLE IF NOT EXISTS daily (
    day  TEXT NOT NULL,      -- "2006-01-02"
    app  TEXT NOT NULL,
    rx   INTEGER NOT NULL DEFAULT 0,
    tx   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, app)
);
CREATE INDEX IF NOT EXISTS idx_hourly_hour ON hourly(hour);
CREATE INDEX IF NOT EXISTS idx_daily_day ON daily(day);
`)
	return err
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Delta is one app's traffic increment in a flush window.
type Delta struct {
	App string
	Rx  uint64
	Tx  uint64
}

// Flush adds deltas to the hour and day buckets for t.
func (d *DB) Flush(t time.Time, deltas []Delta) error {
	if len(deltas) == 0 {
		return nil
	}
	hour := t.Format("2006-01-02T15")
	day := t.Format("2006-01-02")
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hstmt, err := tx.Prepare(`INSERT INTO hourly(hour,app,rx,tx) VALUES(?,?,?,?)
		ON CONFLICT(hour,app) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx`)
	if err != nil {
		return err
	}
	defer hstmt.Close()
	dstmt, err := tx.Prepare(`INSERT INTO daily(day,app,rx,tx) VALUES(?,?,?,?)
		ON CONFLICT(day,app) DO UPDATE SET rx=rx+excluded.rx, tx=tx+excluded.tx`)
	if err != nil {
		return err
	}
	defer dstmt.Close()
	for _, dl := range deltas {
		if dl.Rx == 0 && dl.Tx == 0 {
			continue
		}
		if _, err := hstmt.Exec(hour, dl.App, dl.Rx, dl.Tx); err != nil {
			return err
		}
		if _, err := dstmt.Exec(day, dl.App, dl.Rx, dl.Tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune deletes hourly rows older than hourlyKeep and daily rows older than dailyKeep.
func (d *DB) Prune(now time.Time, hourlyKeep, dailyKeep time.Duration) error {
	hourCut := now.Add(-hourlyKeep).Format("2006-01-02T15")
	dayCut := now.Add(-dailyKeep).Format("2006-01-02")
	if _, err := d.db.Exec(`DELETE FROM hourly WHERE hour < ?`, hourCut); err != nil {
		return err
	}
	_, err := d.db.Exec(`DELETE FROM daily WHERE day < ?`, dayCut)
	return err
}

// AppSum is an aggregated per-app total.
type AppSum struct {
	App   string
	Rx    uint64
	Tx    uint64
}

// HourlyPoint is one hour bucket for an app (or all apps when App=="").
type HourlyPoint struct {
	Hour string
	App  string
	Rx   uint64
	Tx   uint64
}

// DailyPoint is one day bucket.
type DailyPoint struct {
	Day string
	App string
	Rx  uint64
	Tx  uint64
}

// QueryApps sums traffic per app in [from, to) (local time range).
// It reads hourly rows for the recent range and daily rows for the older part,
// so long ranges stay cheap after pruning.
func (d *DB) QueryApps(from, to time.Time) ([]AppSum, error) {
	// Use daily table for whole days, hourly for partial edges.
	// Simplify: query hourly for anything within retention, daily for older.
	hourCut := time.Now().Add(-8 * 24 * time.Hour)
	sums := map[string]*AppSum{}

	// older part via daily
	if from.Before(hourCut) {
		end := to
		if end.After(hourCut) {
			end = hourCut
		}
		rows, err := d.db.Query(`SELECT app, SUM(rx), SUM(tx) FROM daily
			WHERE day >= ? AND day < ? GROUP BY app`,
			from.Format("2006-01-02"), end.Format("2006-01-02"))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var s AppSum
			if err := rows.Scan(&s.App, &s.Rx, &s.Tx); err != nil {
				return nil, err
			}
			sums[s.App] = &s
		}
		rows.Close()
	}

	// recent part via hourly
	recentFrom := from
	if recentFrom.Before(hourCut) {
		recentFrom = hourCut
	}
	if recentFrom.Before(to) {
		rows, err := d.db.Query(`SELECT app, SUM(rx), SUM(tx) FROM hourly
			WHERE hour >= ? AND hour <= ? GROUP BY app`,
			recentFrom.Format("2006-01-02T15"), to.Format("2006-01-02T15"))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var s AppSum
			if err := rows.Scan(&s.App, &s.Rx, &s.Tx); err != nil {
				return nil, err
			}
			if e, ok := sums[s.App]; ok {
				e.Rx += s.Rx
				e.Tx += s.Tx
			} else {
				sums[s.App] = &s
			}
		}
		rows.Close()
	}

	out := make([]AppSum, 0, len(sums))
	for _, s := range sums {
		out = append(out, *s)
	}
	return out, nil
}

// QueryAppHourly returns hour buckets for one app in [from, to).
func (d *DB) QueryAppHourly(app string, from, to time.Time) ([]HourlyPoint, error) {
	rows, err := d.db.Query(`SELECT hour, rx, tx FROM hourly
		WHERE app = ? AND hour >= ? AND hour < ? ORDER BY hour`,
		app, from.Format("2006-01-02T15"), to.Format("2006-01-02T15"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourlyPoint
	for rows.Next() {
		var p HourlyPoint
		if err := rows.Scan(&p.Hour, &p.Rx, &p.Tx); err != nil {
			return nil, err
		}
		p.App = app
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueryAppDaily returns day buckets for one app in [from, to).
func (d *DB) QueryAppDaily(app string, from, to time.Time) ([]DailyPoint, error) {
	rows, err := d.db.Query(`SELECT day, rx, tx FROM daily
		WHERE app = ? AND day >= ? AND day < ? ORDER BY day`,
		app, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyPoint
	for rows.Next() {
		var p DailyPoint
		if err := rows.Scan(&p.Day, &p.Rx, &p.Tx); err != nil {
			return nil, err
		}
		p.App = app
		out = append(out, p)
	}
	return out, rows.Err()
}

// Earliest returns the oldest timestamp present (for "all time" view).
func (d *DB) Earliest() (time.Time, error) {
	var day sql.NullString
	err := d.db.QueryRow(`SELECT MIN(day) FROM daily`).Scan(&day)
	if err != nil {
		return time.Time{}, err
	}
	if !day.Valid {
		return time.Now(), nil
	}
	return time.ParseInLocation("2006-01-02", day.String, time.Local)
}
