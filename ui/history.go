// Package ui implements the bubbletea TUI for browsing historical usage.
package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"traffic-manager/app"
	"traffic-manager/storage"
)

// Period is a selectable time range.
type Period struct {
	Name string
	From func(now time.Time) time.Time
}

// Periods shown as tabs.
var Periods = []Period{
	{"今天", func(now time.Time) time.Time { return dayStart(now) }},
	{"昨天", func(now time.Time) time.Time { return dayStart(now).AddDate(0, 0, -1) }},
	{"近7天", func(now time.Time) time.Time { return dayStart(now).AddDate(0, 0, -6) }},
	{"近30天", func(now time.Time) time.Time { return dayStart(now).AddDate(0, 0, -29) }},
	{"全部", func(now time.Time) time.Time { return time.Time{} }}, // zero = earliest in db
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// HistoryModel browses persisted traffic history.
type HistoryModel struct {
	db       *storage.DB
	dbPath   string
	period   int
	table    table.Model
	detailVP viewport.Model
	mode     viewMode
	sums     []storage.AppSum
	detail   string // app name in detail view
	width    int
	height   int
	err      error
	quitting bool
}

// NewHistory creates the history browsing model.
func NewHistory(db *storage.DB, dbPath string) HistoryModel {
	cols := []table.Column{
		{Title: "APP", Width: 24},
		{Title: "下载 (RX)", Width: 14},
		{Title: "上传 (TX)", Width: 14},
		{Title: "总计", Width: 14},
		{Title: "占比", Width: 8},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true), table.WithHeight(12))
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
	return HistoryModel{db: db, dbPath: dbPath, table: t, width: 100, height: 30}
}

func (m HistoryModel) Init() tea.Cmd {
	return tickCmd()
}

func (m HistoryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "esc":
			if m.mode == viewDetail {
				m.mode = viewList
			}
			return m, nil
		case "tab", "right", "l":
			m.period = (m.period + 1) % len(Periods)
			m.refreshTable()
			return m, nil
		case "shift+tab", "left", "h":
			m.period = (m.period - 1 + len(Periods)) % len(Periods)
			m.refreshTable()
			return m, nil
		case "1", "2", "3", "4", "5":
			m.period = int(msg.String()[0] - '1')
			m.refreshTable()
			return m, nil
		case "enter":
			if m.mode == viewList && len(m.sums) > 0 {
				row := m.table.SelectedRow()
				if len(row) > 0 {
					m.detail = row[0]
					m.mode = viewDetail
					m.detailVP = viewport.New(m.width, max(m.height-7, 5))
					m.refreshDetail()
				}
			}
			return m, nil
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(msg.Height-9, 3))
		m.detailVP.Width = msg.Width
		m.detailVP.Height = max(msg.Height-7, 5)
		return m, nil
	case tickMsg:
		if m.mode == viewList {
			m.refreshTable()
		} else {
			m.refreshDetail()
		}
		return m, tickCmd()
	}
	if m.mode == viewDetail {
		m.detailVP, cmd = m.detailVP.Update(msg)
	} else {
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

func (m *HistoryModel) periodRange() (time.Time, time.Time) {
	now := time.Now()
	from := Periods[m.period].From(now)
	if from.IsZero() {
		if e, err := m.db.Earliest(); err == nil {
			from = e
		} else {
			from = dayStart(now)
		}
	}
	to := now
	if m.period == 1 { // 昨天: end at today's start
		to = dayStart(now)
	}
	return from, to
}

func (m *HistoryModel) refreshTable() {
	from, to := m.periodRange()
	sums, err := m.db.QueryApps(from, to)
	if err != nil {
		m.err = err
		return
	}
	m.err = nil
	sort.Slice(sums, func(i, j int) bool {
		return sums[i].Rx+sums[i].Tx > sums[j].Rx+sums[j].Tx
	})
	m.sums = sums

	var total uint64
	for _, s := range sums {
		total += s.Rx + s.Tx
	}
	rows := make([]table.Row, 0, len(sums))
	for _, s := range sums {
		pct := ""
		if total > 0 {
			pct = fmt.Sprintf("%.1f%%", float64(s.Rx+s.Tx)/float64(total)*100)
		}
		rows = append(rows, table.Row{
			s.App,
			humanBytes(s.Rx),
			humanBytes(s.Tx),
			humanBytes(s.Rx + s.Tx),
			pct,
		})
	}
	m.table.SetRows(rows)
}

func (m *HistoryModel) refreshDetail() {
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "应用: %s    时段: %s\n\n", m.detail, Periods[m.period].Name)

	// hourly bars for today/yesterday, daily bars otherwise
	if m.period <= 1 {
		day := dayStart(now)
		if m.period == 1 {
			day = day.AddDate(0, 0, -1)
		}
		pts, err := m.db.QueryAppHourly(m.detail, day, day.AddDate(0, 0, 1))
		if err != nil {
			b.WriteString("query error: " + err.Error())
			m.detailVP.SetContent(b.String())
			return
		}
		byHour := map[int]storage.HourlyPoint{}
		for _, p := range pts {
			var h int
			fmt.Sscanf(p.Hour, "%*d-%*d-%*dT%d", &h)
			byHour[h] = p
		}
		b.WriteString("每小时用量:\n")
		for h := 0; h < 24; h++ {
			p := byHour[h]
			fmt.Fprintf(&b, "  %02d:00  %-10s %-10s %s\n",
				h, humanBytes(p.Rx), humanBytes(p.Tx), bar(p.Rx+p.Tx, maxHour(byHour), 40))
		}
	} else {
		from, to := m.periodRange()
		pts, err := m.db.QueryAppDaily(m.detail, from, to)
		if err != nil {
			b.WriteString("query error: " + err.Error())
			m.detailVP.SetContent(b.String())
			return
		}
		var maxv uint64
		for _, p := range pts {
			if p.Rx+p.Tx > maxv {
				maxv = p.Rx + p.Tx
			}
		}
		b.WriteString("每天用量:\n")
		for _, p := range pts {
			fmt.Fprintf(&b, "  %s  %-10s %-10s %s\n",
				p.Day, humanBytes(p.Rx), humanBytes(p.Tx), bar(p.Rx+p.Tx, maxv, 40))
		}
		if len(pts) == 0 {
			b.WriteString("  (无数据)\n")
		}
	}
	m.detailVP.SetContent(b.String())
}

func maxHour(m map[int]storage.HourlyPoint) uint64 {
	var maxv uint64
	for _, p := range m {
		if p.Rx+p.Tx > maxv {
			maxv = p.Rx + p.Tx
		}
	}
	return maxv
}

func bar(v, maxv uint64, width int) string {
	if maxv == 0 || v == 0 {
		return ""
	}
	n := int(float64(v) / float64(maxv) * float64(width))
	if n < 1 {
		n = 1
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Render(strings.Repeat("█", n))
}

// View renders the history UI.
func (m HistoryModel) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).
		Render("traffic-manager · 历史用量")
	b.WriteString(title + "  ")
	// tabs
	for i, p := range Periods {
		st := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
		if i == m.period {
			st = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).
				Background(lipgloss.Color("57")).Padding(0, 1)
		} else {
			st = st.Padding(0, 1)
		}
		b.WriteString(st.Render(p.Name))
	}
	b.WriteString("\n")

	// summary line
	var totalRx, totalTx uint64
	for _, s := range m.sums {
		totalRx += s.Rx
		totalTx += s.Tx
	}
	var fresh string
	if pid := app.DaemonPID(m.dbPath); pid > 0 {
		fresh = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).
			Render(fmt.Sprintf("daemon 运行中 (pid %d)", pid))
	} else {
		fresh = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).
			Render("daemon 未运行，数据不会更新（启动: traffic-manager daemon）")
	}
	fmt.Fprintf(&b, "合计: ↓%s ↑%s 共 %s  ·  %d 个应用  ·  %s\n",
		humanBytes(totalRx), humanBytes(totalTx), humanBytes(totalRx+totalTx),
		len(m.sums), fresh)
	if m.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("196")).
			Render("error: "+m.err.Error()) + "\n")
	}
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render(strings.Repeat("─", min(m.width, 90))) + "\n")

	if m.mode == viewDetail {
		b.WriteString(m.detailVP.View())
		b.WriteString("\n" + helpStyle().Render("esc: 返回  q: 退出"))
		return b.String()
	}
	b.WriteString(m.table.View())
	b.WriteString("\n" + helpStyle().Render(
		"tab/←→: 切换时段  enter: 应用明细  1-5: 快捷时段  q: 退出"))
	return b.String()
}
