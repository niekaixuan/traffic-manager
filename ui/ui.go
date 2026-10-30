// Package ui implements the bubbletea TUI.
package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"traffic-manager/app"
	"traffic-manager/stats"
)

type tickMsg time.Time

type sortMode int

const (
	sortByTotal sortMode = iota
	sortByRx
	sortByTx
	sortByName
	sortByPID
)

var sortNames = map[sortMode]string{
	sortByTotal: "total",
	sortByRx:    "↓rx",
	sortByTx:    "↑tx",
	sortByName:  "name",
	sortByPID:   "pid",
}

type viewMode int

const (
	viewList viewMode = iota
	viewDetail
)

// Model is the bubbletea model.
type Model struct {
	mon      *app.Monitor
	ifaces   []string
	table    table.Model
	detailVP viewport.Model
	spinner  spinner.Model
	mode     viewMode
	sort     sortMode
	width    int
	height   int
	apps     []stats.App
	detailPID int32
	packets  uint64
	dropped  uint64
	started  time.Time
	quitting bool
}

// New creates the model.
func New(mon *app.Monitor, ifaces []string) Model {
	cols := []table.Column{
		{Title: "PID", Width: 7},
		{Title: "APP", Width: 22},
		{Title: "RX/s", Width: 12},
		{Title: "TX/s", Width: 12},
		{Title: "RX TOTAL", Width: 12},
		{Title: "TX TOTAL", Width: 12},
		{Title: "TOTAL", Width: 12},
		{Title: "FLOWS", Width: 6},
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithHeight(10),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t.SetStyles(s)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return Model{
		mon:     mon,
		ifaces:  ifaces,
		table:   t,
		spinner: sp,
		sort:    sortByTotal,
		started: time.Now(),
		width:   100,
		height:  30,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			if m.mode == viewDetail {
				m.mode = viewList
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		case "s":
			m.sort = (m.sort + 1) % 5
			return m, nil
		case "enter":
			if m.mode == viewList && len(m.apps) > 0 {
				row := m.table.SelectedRow()
				if len(row) > 0 {
					var pid int32
					fmt.Sscanf(row[0], "%d", &pid)
					m.detailPID = pid
					m.mode = viewDetail
					m.detailVP = viewport.New(m.width, m.height-6)
					m.refreshDetail()
				}
			}
			return m, nil
		case "r":
			// reverse? no—reserved. ignore
			return m, nil
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(msg.Height-8, 3))
		m.detailVP.Width = msg.Width
		m.detailVP.Height = max(msg.Height-6, 3)
		return m, nil
	case tickMsg:
		m.refreshTable()
		if m.mode == viewDetail {
			m.refreshDetail()
		}
		m.packets, m.dropped = m.mon.CaptureStats()
		return m, tickCmd()
	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	if m.mode == viewDetail {
		m.detailVP, cmd = m.detailVP.Update(msg)
	} else {
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

func (m *Model) refreshTable() {
	apps := m.mon.Store.Snapshot()
	m.sortApps(apps)
	m.apps = apps

	rows := make([]table.Row, 0, len(apps))
	for _, a := range apps {
		name := a.Name
		if name == "" {
			name = "?"
		}
		rows = append(rows, table.Row{
			fmt.Sprintf("%d", a.PID),
			truncate(name, 20),
			humanRate(a.RxRate),
			humanRate(a.TxRate),
			humanBytes(a.RxBytes),
			humanBytes(a.TxBytes),
			humanBytes(a.RxBytes + a.TxBytes),
			fmt.Sprintf("%d", a.Flows),
		})
	}
	m.table.SetRows(rows)
}

func (m *Model) sortApps(apps []stats.App) {
	switch m.sort {
	case sortByTotal:
		sort.Slice(apps, func(i, j int) bool {
			return apps[i].RxBytes+apps[i].TxBytes > apps[j].RxBytes+apps[j].TxBytes
		})
	case sortByRx:
		sort.Slice(apps, func(i, j int) bool { return apps[i].RxRate > apps[j].RxRate })
	case sortByTx:
		sort.Slice(apps, func(i, j int) bool { return apps[i].TxRate > apps[j].TxRate })
	case sortByName:
		sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	case sortByPID:
		sort.Slice(apps, func(i, j int) bool { return apps[i].PID < apps[j].PID })
	}
}

func (m *Model) refreshDetail() {
	flows := m.mon.Store.FlowsFor(m.detailPID)
	sort.Slice(flows, func(i, j int) bool {
		return flows[i].RxBytes+flows[i].TxBytes > flows[j].RxBytes+flows[j].TxBytes
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Process PID %d — %d flows\n\n", m.detailPID, len(flows))
	fmt.Fprintf(&b, "%-6s %-28s %14s %14s\n", "PROTO", "REMOTE", "RX", "TX")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("─", 66))
	for _, f := range flows {
		proto := "tcp"
		if f.Proto == 17 {
			proto = "udp"
		}
		remote := fmt.Sprintf("%s:%d", f.RemoteIP, f.RemotePort)
		fmt.Fprintf(&b, "%-6s %-28s %14s %14s\n",
			proto, truncate(remote, 28), humanBytes(f.RxBytes), humanBytes(f.TxBytes))
	}
	m.detailVP.SetContent(b.String())
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder

	total := m.mon.Store.TotalSnapshot()
	elapsed := time.Since(m.started).Round(time.Second)

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).
		Render("traffic-manager")
	fmt.Fprintf(&b, "%s %s  ifaces: %s  uptime: %s  sort: %s\n",
		m.spinner.View(), title, strings.Join(m.ifaces, ","), elapsed, sortNames[m.sort])
	fmt.Fprintf(&b, "total: ↓%s ↑%s   rate: ↓%s ↑%s   pkts: %d unmatched: %d\n",
		humanBytes(total.RxBytes), humanBytes(total.TxBytes),
		humanRate(total.RxRate), humanRate(total.TxRate),
		m.packets, m.dropped)
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render(strings.Repeat("─", min(m.width, 90))) + "\n")

	if m.mode == viewDetail {
		b.WriteString(m.detailVP.View())
		b.WriteString("\n" + helpStyle().Render("esc: back  q: quit"))
		return b.String()
	}

	b.WriteString(m.table.View())
	b.WriteString("\n" + helpStyle().Render(
		"↑/↓: move  enter: flows  s: sort  q: quit"))
	return b.String()
}

func helpStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func humanBytes(n uint64) string {
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

func humanRate(r float64) string {
	if r < 1 {
		return "0B/s"
	}
	return humanBytes(uint64(r)) + "/s"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
