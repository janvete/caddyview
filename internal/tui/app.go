package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/janvete/caddyview/internal/parser"
	"github.com/janvete/caddyview/internal/sshclient"
)

// -- styles -------------------------------------------------------------------

var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 1)

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39"))

	styleGood = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleBad  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))

	styleDim = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// -- messages -----------------------------------------------------------------

type tickMsg time.Time
type newLineMsg string
type sysStatsMsg struct {
	cpu          float64
	memUsed      int64
	memTotal     int64
}
type errMsg struct{ err error }

// -- model --------------------------------------------------------------------

type model struct {
	client    *sshclient.Client
	logPath   string
	lineCh    chan string
	doneCh    chan struct{}

	liveEntries []*parser.LogEntry
	liveStats   *parser.Stats
	liveStart   time.Time

	cpu      float64
	memUsed  int64
	memTotal int64

	width  int
	height int
	err    error
}

func initialModel(client *sshclient.Client, logPath string) model {
	return model{
		client:    client,
		logPath:   logPath,
		lineCh:    make(chan string, 256),
		doneCh:    make(chan struct{}),
		liveStats: parser.NewStats(),
		liveStart: time.Now(),
		width:     120,
		height:    40,
	}
}

// -- init ---------------------------------------------------------------------

func (m model) Init() tea.Cmd {
	return tea.Batch(
		startStreaming(m.client, m.logPath, m.lineCh, m.doneCh),
		tick(),
		fetchSysStats(m.client),
	)
}

func startStreaming(client *sshclient.Client, logPath string, ch chan string, done chan struct{}) tea.Cmd {
	return func() tea.Msg {
		if err := client.StreamLines(logPath, ch, done); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func fetchSysStats(client *sshclient.Client) tea.Cmd {
	return func() tea.Msg {
		cpu, used, total, err := client.SysStats()
		if err != nil {
			return sysStatsMsg{}
		}
		return sysStatsMsg{cpu: cpu, memUsed: used, memTotal: total}
	}
}

func drainLines(ch chan string) tea.Cmd {
	return func() tea.Msg {
		select {
		case line := <-ch:
			return newLineMsg(line)
		default:
			return nil
		}
	}
}

// -- update -------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			close(m.doneCh)
			return m, tea.Quit
		}

	case tickMsg:
		return m, tea.Batch(tick(), drainLines(m.lineCh), fetchSysStats(m.client))

	case newLineMsg:
		if e := parser.ParseLine(string(msg)); e != nil {
			m.liveStats.Add(e)
			m.liveEntries = append(m.liveEntries, e)
			if len(m.liveEntries) > 10000 {
				m.liveEntries = m.liveEntries[len(m.liveEntries)-10000:]
			}
		}
		return m, drainLines(m.lineCh)

	case sysStatsMsg:
		m.cpu = msg.cpu
		m.memUsed = msg.memUsed
		m.memTotal = msg.memTotal

	case errMsg:
		m.err = msg.err
	}

	return m, nil
}

// -- view ---------------------------------------------------------------------

func (m model) View() string {
	if m.err != nil {
		return styleBad.Render(fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err))
	}

	header := m.renderHeader()
	stats := m.renderStats()
	hosts := m.renderTopHosts()
	ips := m.renderTopIPs()
	status := m.renderStatusCodes()
	footer := styleDim.Render("  q quit")

	top := lipgloss.JoinHorizontal(lipgloss.Top, stats, hosts, ips, status)
	return lipgloss.JoinVertical(lipgloss.Left, header, top, footer)
}

func (m model) renderHeader() string {
	elapsed := time.Since(m.liveStart).Round(time.Second)
	cpuColor := styleGood
	if m.cpu > 80 {
		cpuColor = styleBad
	} else if m.cpu > 50 {
		cpuColor = styleWarn
	}

	memPct := 0.0
	if m.memTotal > 0 {
		memPct = float64(m.memUsed) / float64(m.memTotal) * 100
	}
	memColor := styleGood
	if memPct > 80 {
		memColor = styleBad
	} else if memPct > 60 {
		memColor = styleWarn
	}

	parts := []string{
		styleTitle.Render("caddyview"),
		styleDim.Render("  "),
		styleHeader.Render("host: ") + m.client.Host(),
		styleDim.Render("  "),
		styleHeader.Render("log: ") + styleDim.Render(m.logPath),
		styleDim.Render("  "),
		styleHeader.Render("uptime: ") + elapsed.String(),
		styleDim.Render("  "),
		styleHeader.Render("CPU: ") + cpuColor.Render(fmt.Sprintf("%.1f%%", m.cpu)),
		styleDim.Render("  "),
		styleHeader.Render("MEM: ") + memColor.Render(fmt.Sprintf("%d/%d MB (%.0f%%)", m.memUsed, m.memTotal, memPct)),
	}

	return lipgloss.NewStyle().
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("62")).
		Width(m.width).
		Render(strings.Join(parts, ""))
}

func (m model) renderStats() string {
	elapsed := time.Since(m.liveStart).Seconds()
	rps := float64(m.liveStats.TotalRequests)
	if elapsed > 0 {
		rps = float64(m.liveStats.TotalRequests) / elapsed
	}
	bps := m.liveStats.BytesPerSec(elapsed)

	content := strings.Join([]string{
		styleHeader.Render("Live Stats"),
		"",
		fmt.Sprintf("Requests  : %s", styleGood.Render(fmt.Sprintf("%d", m.liveStats.TotalRequests))),
		fmt.Sprintf("Req/sec   : %s", styleGood.Render(fmt.Sprintf("%.2f", rps))),
		fmt.Sprintf("Transfer  : %s", formatBytes(m.liveStats.TotalBytes)),
		fmt.Sprintf("Speed     : %s", formatBytes(int64(bps))+"/s"),
		fmt.Sprintf("Avg resp  : %.1fms", m.liveStats.AvgDurationMs),
	}, "\n")

	return styleBorder.Render(content)
}

func (m model) renderTopHosts() string {
	lines := []string{styleHeader.Render("Top Hosts"), ""}
	type kv struct {
		k string
		v int64
	}
	pairs := make([]kv, 0, len(m.liveStats.TopHosts))
	for k, v := range m.liveStats.TopHosts {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	for _, p := range pairs {
		host := p.k
		if len(host) > 30 {
			host = host[:27] + "..."
		}
		lines = append(lines, fmt.Sprintf("%-30s %s", host, styleGood.Render(fmt.Sprintf("%d", p.v))))
	}
	if len(pairs) == 0 {
		lines = append(lines, styleDim.Render("waiting for requests..."))
	}
	return styleBorder.Render(strings.Join(lines, "\n"))
}

func (m model) renderTopIPs() string {
	lines := []string{styleHeader.Render("Top IPs"), ""}
	type kv struct {
		k string
		v int64
	}
	pairs := make([]kv, 0, len(m.liveStats.TopIPs))
	for k, v := range m.liveStats.TopIPs {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	for _, p := range pairs {
		lines = append(lines, fmt.Sprintf("%-20s %s", p.k, styleGood.Render(fmt.Sprintf("%d", p.v))))
	}
	if len(pairs) == 0 {
		lines = append(lines, styleDim.Render("waiting for requests..."))
	}
	return styleBorder.Render(strings.Join(lines, "\n"))
}

func (m model) renderStatusCodes() string {
	lines := []string{styleHeader.Render("Status Codes"), ""}

	groups := map[string]int64{
		"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0,
	}
	for code, count := range m.liveStats.StatusCodes {
		switch {
		case code >= 200 && code < 300:
			groups["2xx"] += count
		case code >= 300 && code < 400:
			groups["3xx"] += count
		case code >= 400 && code < 500:
			groups["4xx"] += count
		case code >= 500:
			groups["5xx"] += count
		}
	}

	colorFor := func(g string, v int64) string {
		s := fmt.Sprintf("%d", v)
		switch g {
		case "2xx":
			return styleGood.Render(s)
		case "3xx":
			return styleDim.Render(s)
		case "4xx":
			return styleWarn.Render(s)
		case "5xx":
			return styleBad.Render(s)
		}
		return s
	}

	for _, g := range []string{"2xx", "3xx", "4xx", "5xx"} {
		lines = append(lines, fmt.Sprintf("%s : %s", g, colorFor(g, groups[g])))
	}
	return styleBorder.Render(strings.Join(lines, "\n"))
}

// -- helpers ------------------------------------------------------------------

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// -- entry point --------------------------------------------------------------

func Run(client *sshclient.Client, logPath string) error {
	p := tea.NewProgram(initialModel(client, logPath), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
