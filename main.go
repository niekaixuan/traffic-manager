package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
		case "install":
			runInstall()
			return
		case "help", "-h", "--help", "-help":
			printUsage()
			return
		}
	}
	// default: history browser
	runHistory(os.Args[1:])
}

func printUsage() {
	fmt.Print(`traffic-manager — 按应用统计本机网络流量（nethogs + vnStat）

用法:
  traffic-manager [命令] [选项]

命令:
  (默认)      打开历史用量 TUI，浏览今天/昨天/近7天/近30天/全部
  daemon      后台采集：抓包并每 30 秒写入数据库（需要 root 或 capability）
  live        实时监控 TUI：各进程当前的上下行速率（需要 root 或 capability）
  query       输出文本报表，适合脚本或快速查看
  list        列出本机网卡
  install     安装二进制和 systemd 服务并立即启动（需要 root）
  help        显示本帮助

选项:
  daemon, live:
    -i <网卡>     监听的网卡，逗号分隔（默认：所有 up 且非 loopback 的网卡）
  daemon:
    -background   脱离终端在后台运行（日志写到 <db目录>/daemon.log）
  daemon, 默认命令, query:
    -db <路径>    数据库路径（默认 root: /var/lib/traffic-manager/traffic.db，
                  普通用户: ~/.local/share/traffic-manager/traffic.db）
  query:
    -days <n>     统计最近 n 天（默认 1，即今天）

典型用法:
  sudo ./traffic-manager daemon              # 前台运行采集
  sudo ./traffic-manager daemon -background  # 后台运行采集
  ./traffic-manager                          # 查看历史用量（不需要 root）
  ./traffic-manager query -days 7            # 最近 7 天各应用用量
  sudo ./traffic-manager live                # 实时监控
  sudo ./traffic-manager install             # 装成 systemd 服务，开机自启

免 sudo 运行（一次性授权）:
  sudo setcap cap_net_raw,cap_net_admin,cap_sys_ptrace=eip ./traffic-manager

历史 TUI 按键:
  tab / ←→     切换时段    1-5    快捷选择时段
  ↑/↓, j/k     移动选择    enter  应用明细（逐小时/逐天柱状图）
  esc          返回        q      退出
`)
}

// runHistory opens the DB read-only and shows the history TUI.
func runHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	dbPath := fs.String("db", storage.DefaultPath(), "database path")
	_ = fs.Parse(args)

	db, err := storage.OpenReader(*dbPath)
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

	db, err := storage.OpenReader(*dbPath)
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
	bg := fs.Bool("background", false, "detach and run in the background")
	_ = fs.Parse(args)

	if *bg {
		runDaemonBackground(*ifaceFlag, *dbPath)
		return
	}

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

// runDaemonBackground re-executes the daemon detached from the terminal
// (new session, stdio redirected to a log file) and exits.
func runDaemonBackground(ifaceFlag, dbPath string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	exe, _ = filepath.Abs(exe)

	logDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	logPath := filepath.Join(logDir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	daemonArgs := []string{"daemon", "-db", dbPath}
	if ifaceFlag != "" {
		daemonArgs = append(daemonArgs, "-i", ifaceFlag)
	}
	devNull, _ := os.Open(os.DevNull)
	defer devNull.Close()

	attr := &os.ProcAttr{
		Files: []*os.File{devNull, logFile, logFile},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	}
	proc, err := os.StartProcess(exe, append([]string{exe}, daemonArgs...), attr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	pid := proc.Pid
	_ = proc.Release()
	fmt.Printf("daemon running in background (pid %d)\n  db:  %s\n  log: %s\n",
		pid, dbPath, logPath)
	fmt.Printf("stop with: kill %d\n", pid)
}

// runInstall installs the binary and a systemd unit, then enables it.
func runInstall() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "install needs root: sudo ./traffic-manager install")
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	exe, _ = filepath.Abs(exe)

	const binDst = "/usr/local/bin/traffic-manager"
	src, err := os.Open(exe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer src.Close()
	tmp := binDst + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dst.Close()
	if err := os.Rename(tmp, binDst); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("installed", binDst)

	unit := `[Unit]
Description=traffic-manager per-app network usage collector
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/traffic-manager daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`
	const unitPath = "/etc/systemd/system/traffic-manager.service"
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("installed", unitPath)

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", "traffic-manager"},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "systemctl %s: %v\n", strings.Join(args, " "), err)
			os.Exit(1)
		}
	}
	fmt.Println("service enabled and started; check: systemctl status traffic-manager")
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
