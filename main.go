package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"traffic-manager/app"
	"traffic-manager/stats"
	"traffic-manager/storage"
	"traffic-manager/ui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			runDaemon(os.Args[2:])
			return
		case "live":
			runLive(os.Args[2:])
			return
		case "query":
			runQuery(os.Args[2:])
			return
		case "list":
			listIfaces()
			return
		}
	}
	// default: history browser
	runHistory(os.Args[1:])
}

// runHistory opens the DB read-only and shows the history TUI.
func runHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	dbPath := fs.String("db", storage.DefaultPath(), "database path")
	_ = fs.Parse(args)

	db, err := storage.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	p := tea.NewProgram(ui.NewHistory(db, *dbPath), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runQuery prints a text report (no TUI).
func runQuery(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	dbPath := fs.String("db", storage.DefaultPath(), "database path")
	days := fs.Int("days", 1, "how many past days to include")
	_ = fs.Parse(args)

	db, err := storage.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	now := time.Now()
	y, m, d := now.Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(*days-1))
	sums, err := db.QueryApps(from, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	sort.Slice(sums, func(i, j int) bool { return sums[i].Rx+sums[i].Tx > sums[j].Rx+sums[j].Tx })

	fmt.Printf("%-24s %14s %14s %14s\n", "APP", "RX", "TX", "TOTAL")
	var totRx, totTx uint64
	for _, s := range sums {
		fmt.Printf("%-24s %14s %14s %14s\n", s.App,
			human(s.Rx), human(s.Tx), human(s.Rx+s.Tx))
		totRx += s.Rx
		totTx += s.Tx
	}
	fmt.Printf("%-24s %14s %14s %14s\n", "TOTAL", human(totRx), human(totTx), human(totRx+totTx))
}

func human(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// runDaemon starts the persistent collector.
func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	ifaceFlag := fs.String("i", "", "comma-separated interfaces (default: all up, non-loopback)")
	dbPath := fs.String("db", storage.DefaultPath(), "database path")
	_ = fs.Parse(args)

	if !hasNetRaw() {
		fmt.Fprintln(os.Stderr, "daemon needs root or CAP_NET_RAW:")
		fmt.Fprintln(os.Stderr, "  sudo ./traffic-manager daemon")
		fmt.Fprintln(os.Stderr, "  or: sudo setcap cap_net_raw,cap_net_admin=eip ./traffic-manager")
		os.Exit(1)
	}

	ifaces, err := pickIfaces(*ifaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	db, err := storage.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	store := stats.New()
	mon, err := app.NewMonitor(store, ifaces)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("capturing on %s, db=%s", strings.Join(ifaces, ","), *dbPath)
	if err := app.RunDaemon(ctx, mon, db); err != nil && err != context.Canceled {
		log.Printf("daemon: %v", err)
	}
}

// runLive is the original realtime TUI.
func runLive(args []string) {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	ifaceFlag := fs.String("i", "", "comma-separated interfaces (default: all up, non-loopback)")
	_ = fs.Parse(args)

	ifaces, err := pickIfaces(*ifaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if !hasNetRaw() {
		fmt.Fprintln(os.Stderr, "warning: missing CAP_NET_RAW; packet capture requires root or capabilities.")
		fmt.Fprintln(os.Stderr, "  run: sudo ./traffic-manager live")
		fmt.Fprintln(os.Stderr, "  or:  sudo setcap cap_net_raw,cap_net_admin=eip ./traffic-manager")
	}

	store := stats.New()
	mon, err := app.NewMonitor(store, ifaces)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mon.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	p := tea.NewProgram(ui.New(mon, ifaces), tea.WithAltScreen())
	go func() {
		<-ctx.Done()
		p.Kill()
	}()
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	mon.Stop()
}

func listIfaces() {
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		addrs, _ := ifi.Addrs()
		var ss []string
		for _, a := range addrs {
			ss = append(ss, a.String())
		}
		fmt.Printf("%-12s flags=%-10s addrs=%s\n", ifi.Name, ifi.Flags, strings.Join(ss, " "))
	}
}

// pickIfaces returns requested interfaces or all up non-loopback ones.
func pickIfaces(spec string) ([]string, error) {
	if spec != "" {
		parts := strings.Split(spec, ",")
		for _, p := range parts {
			if _, err := net.InterfaceByName(strings.TrimSpace(p)); err != nil {
				return nil, fmt.Errorf("interface %q: %w", p, err)
			}
		}
		return parts, nil
	}
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		out = append(out, ifi.Name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no up non-loopback interfaces found")
	}
	return out, nil
}

// hasNetRaw reports whether we can open raw packet sockets
// (root or CAP_NET_RAW effective).
func hasNetRaw() bool {
	if os.Geteuid() == 0 {
		return true
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		if err != nil {
			return false
		}
		const capNetRaw = 13
		return v&(1<<capNetRaw) != 0
	}
	return false
}
