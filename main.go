package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"traffic-manager/app"
	"traffic-manager/stats"
	"traffic-manager/ui"
)

func main() {
	var (
		ifaceFlag string
		listFlag  bool
	)
	flag.StringVar(&ifaceFlag, "i", "", "comma-separated interfaces to monitor (default: all up, non-loopback)")
	flag.BoolVar(&listFlag, "list", false, "list interfaces and exit")
	flag.Parse()

	if listFlag {
		ifaces, _ := net.Interfaces()
		for _, ifi := range ifaces {
			addrs, _ := ifi.Addrs()
			var ss []string
			for _, a := range addrs {
				ss = append(ss, a.String())
			}
			fmt.Printf("%-12s flags=%-10s addrs=%s\n", ifi.Name, ifi.Flags, strings.Join(ss, " "))
		}
		return
	}

	ifaces, err := pickIfaces(ifaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if !hasNetRaw() {
		fmt.Fprintln(os.Stderr, "warning: missing CAP_NET_RAW; packet capture requires root or capabilities.")
		fmt.Fprintln(os.Stderr, "  run: sudo ./traffic-manager")
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
	// ensure the TUI exits on SIGTERM/SIGINT context cancellation
	go func() {
		<-ctx.Done()
		p.Kill()
	}()
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	mon.Stop()
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
