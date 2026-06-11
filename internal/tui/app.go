package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/janvete/caddyview/internal/parser"
	"github.com/janvete/caddyview/internal/sshclient"
)

// -- tabs ---------------------------------------------------------------------

type viewTab int

const (
	tabLive  viewTab = 0
	tabDay   viewTab = 1
	tabWeek  viewTab = 2
	tabMonth viewTab = 3
)

var tabLabels = []string{"[1] Live", "[2] 1 Day", "[3] 1 Week", "[4] 1 Month"}

// tabWindow returns how far back each history tab reaches.
func tabWindow(t viewTab) time.Duration {
	switch t {
	case tabDay:
		return 24 * time.Hour
	case tabWeek:
		return 7 * 24 * time.Hour
	case tabMonth:
		return 30 * 24 * time.Hour
	}
	return 0
}

// tabBucketSize returns the bucket granularity for the bar chart.
func tabBucketSize(t viewTab) time.Duration {
	switch t {
	case tabDay:
		return time.Hour
	case tabWeek:
		return 6 * time.Hour
	case tabMonth:
		return 24 * time.Hour
	}
	return time.Hour
}

// -- styles -------------------------------------------------------------------

var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 1)

	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))

	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))

	styleTabActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("16")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	styleTabInactive = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Padding(0, 1)

	styleGood = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleBad  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleBar  = lipgloss.NewStyle().Foreground(lipgloss.Color("62"))
)

// -- history state ------------------------------------------------------------

type historyState struct {
	entries []*parser.LogEntry
	stats   *parser.Stats
	buckets []parser.Bucket
	loaded  bool
	loading bool
	err     error
}

// -- messages -----------------------------------------------------------------

type tickMsg time.Time
type newLineMsg string
type sysStatsMsg struct {
	cpu      float64
	memUsed  int64
	memTotal int64
}
type historyLoadedMsg struct {
	tab     viewTab
	entries []*parser.LogEntry
	err     error
}
type errMsg struct{ err error }

// -- model --------------------------------------------------------------------

type model struct {
	client  *sshclient.Client
	logPath string
	lineCh  chan string
	doneCh  chan struct{}

	liveEntries []*parser.LogEntry
	liveStats   *parser.Stats
	liveStart   time.Time

	activeTab viewTab
	history   [3]*historyState

	cpu      float64
	memUsed  int64
	memTotal int64

	width  int
	height int
	err    error
}

func initialModel(client *sshclient.Client, logPath string) model {
	m := model{
		client:    client,
		logPath:   logPath,
		lineCh:    make(chan string, 256),
		doneCh:    make(chan struct{}),
		liveStats: parser.NewStats(),
		liveStart: time.Now(),
		width:     120,
		height:    40,
	}
	for i := range m.history {
		m.history[i] = &historyState{}
	}
	return m
}

// -- init ---------------------------------------------------------------------

func (m model) Init() tea.Cmd {
	return tea.Batch(
		startStreaming(m.client, m.logPath, m.lineCh, m.doneCh),
		tick(),
		fetchSysStats(m.client),
	)
}

// -- commands -----------------------------------------------------------------

func startStreaming(client *sshclient.Client, logPath string, ch chan string, done chan struct{}) tea.Cmd {
	return func() tea.Msg {
		if err := client.StreamLines(logPath, ch, done); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
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

func loadHistory(client *sshclient.Client, logPath string, tab viewTab) tea.Cmd {
	since := time.Now().Add(-tabWindow(tab)).Unix()
	return func() tea.Msg {
		lines, err := client.LoadHistoricalLines(logPath, since)
		if err != nil {
			return historyLoadedMsg{tab: tab, err: err}
		}
		return historyLoadedMsg{tab: tab, entries: parser.ParseLines(lines)}
	}
}

// -- update -------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			close(m.doneCh)
			return m, tea.Quit
		case "1":
			m.activeTab = tabLive
		case "2":
			m.activeTab = tabDay
			return m, m.ensureHistory(tabDay)
		case "3":
			m.activeTab = tabWeek
			return m, m.ensureHistory(tabWeek)
		case "4":
			m.activeTab = tabMonth
			return m, m.ensureHistory(tabMonth)
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
		m.cpu, m.memUsed, m.memTotal = msg.cpu, msg.memUsed, msg.memTotal

	case historyLoadedMsg:
		idx := int(msg.tab) - 1
		h := m.history[idx]
		h.loading = false
		h.loaded = true
		h.err = msg.err
		if msg.err == nil {
			h.entries = msg.entries
			h.stats = parser.ComputeStats(msg.entries)
			h.buckets = parser.Bucketize(msg.entries, tabBucketSize(msg.tab))
		}

	case errMsg:
		m.err = msg.err
	}

	return m, nil
}

func (m *model) ensureHistory(tab viewTab) tea.Cmd {
	h := m.history[int(tab)-1]
	if h.loaded || h.loading {
		return nil
	}
	h.loading = true
	return loadHistory(m.client, m.logPath, tab)
}

// -- view ---------------------------------------------------------------------

func (m model) View() string {
	if m.err != nil {
		return styleBad.Render(fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err))
	}
	parts := []string{
		m.renderHeader(),
		m.renderTabBar(),
	}
	switch m.activeTab {
	case tabLive:
		parts = append(parts, m.renderLive())
	case tabDay:
		parts = append(parts, m.renderHistory(0, "last 24 hours"))
	case tabWeek:
		parts = append(parts, m.renderHistory(1, "last 7 days"))
	case tabMonth:
		parts = append(parts, m.renderHistory(2, "last 30 days"))
	}
	parts = append(parts, styleDim.Render("  1 live  2 day  3 week  4 month  q quit"))
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m model) renderHeader() string {
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
	row := strings.Join([]string{
		styleTitle.Render("caddyview"),
		"  ",
		styleHeader.Render("host: ") + m.client.Host(),
		"  ",
		styleHeader.Render("log: ") + styleDim.Render(m.logPath),
		"  ",
		styleHeader.Render("up: ") + time.Since(m.liveStart).Round(time.Second).String(),
		"  ",
		styleHeader.Render("CPU: ") + cpuColor.Render(fmt.Sprintf("%.1f%%", m.cpu)),
		"  ",
		styleHeader.Render("MEM: ") + memColor.Render(fmt.Sprintf("%d/%d MB (%.0f%%)", m.memUsed, m.memTotal, memPct)),
	}, "")
	return lipgloss.NewStyle().
		BorderBottom(true).BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("62")).Width(m.width).
		Render(row)
}

func (m model) renderTabBar() string {
	tabs := make([]string, len(tabLabels))
	for i, label := range tabLabels {
		if viewTab(i) == m.activeTab {
			tabs[i] = styleTabActive.Render(label)
		} else {
			tabs[i] = styleTabInactive.Render(label)
		}
	}
	return lipgloss.NewStyle().
		BorderBottom(true).BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("62")).Width(m.width).
		Render(strings.Join(tabs, " "))
}

// -- live view ----------------------------------------------------------------

func (m model) renderLive() string {
	elapsed := time.Since(m.liveStart).Seconds()
	rps := 0.0
	if elapsed > 0 {
		rps = float64(m.liveStats.TotalRequests) / elapsed
	}
	statsBox := styleBorder.Render(strings.Join([]string{
		styleHeader.Render("Live Stats"),
		"",
		fmt.Sprintf("Requests  : %s", styleGood.Render(formatComma(m.liveStats.TotalRequests))),
		fmt.Sprintf("Req/sec   : %s", styleGood.Render(fmt.Sprintf("%.2f", rps))),
		fmt.Sprintf("Transfer  : %s", formatBytes(m.liveStats.TotalBytes)),
		fmt.Sprintf("Speed     : %s/s", formatBytes(int64(m.liveStats.BytesPerSec(elapsed)))),
		fmt.Sprintf("Avg resp  : %.1fms", m.liveStats.AvgDurationMs),
	}, "\n"))
	return lipgloss.JoinHorizontal(lipgloss.Top,
		statsBox,
		m.renderTopHosts(m.liveStats),
		m.renderTopIPs(m.liveStats),
		m.renderStatusCodes(m.liveStats),
	)
}

// -- history view -------------------------------------------------------------

func (m model) renderHistory(idx int, label string) string {
	h := m.history[idx]
	if h.loading {
		return lipgloss.NewStyle().Padding(2, 4).
			Render(styleWarn.Render("⏳ Loading historical data…"))
	}
	if !h.loaded {
		return lipgloss.NewStyle().Padding(2, 4).Render(styleDim.Render("No data loaded."))
	}
	if h.err != nil {
		return lipgloss.NewStyle().Padding(2, 4).
			Render(styleBad.Render(fmt.Sprintf("Error: %v", h.err)))
	}
	if len(h.entries) == 0 {
		return lipgloss.NewStyle().Padding(2, 4).
			Render(styleDim.Render("No log entries found for this period."))
	}

	tab := viewTab(idx + 1)
	chart := m.renderBarChart(h.buckets, tabBucketSize(tab), label, h.stats)
	bottom := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderPeriodStats(h.stats),
		m.renderTopHosts(h.stats),
		m.renderTopIPs(h.stats),
		m.renderStatusCodes(h.stats),
	)
	return lipgloss.JoinVertical(lipgloss.Left, chart, bottom)
}

// -- bar chart ----------------------------------------------------------------

func (m model) renderBarChart(buckets []parser.Bucket, bucketSize time.Duration, label string, stats *parser.Stats) string {
	if len(buckets) == 0 {
		return styleBorder.Width(m.width - 4).Render(styleDim.Render("No data for this period."))
	}

	sorted := make([]parser.Bucket, len(buckets))
	copy(sorted, buckets)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	var maxReqs int64
	for _, b := range sorted {
		if b.Requests > maxReqs {
			maxReqs = b.Requests
		}
	}

	// Pick label format based on bucket size
	timeFmt := "15:04"
	switch {
	case bucketSize >= 24*time.Hour:
		timeFmt = "02 Jan"
	case bucketSize >= 6*time.Hour:
		timeFmt = "Mon 15h"
	}

	labelW := len(timeFmt) + 1
	countW := len(fmt.Sprintf("%d", maxReqs)) + 2
	barMax := m.width - labelW - countW - 8
	if barMax < 10 {
		barMax = 10
	}

	title := fmt.Sprintf("Requests / %s — %s — total: %s req, %s",
		bucketSizeLabel(bucketSize), label,
		formatComma(stats.TotalRequests), formatBytes(stats.TotalBytes),
	)

	rows := []string{styleHeader.Render(title), ""}
	for _, b := range sorted {
		barLen := 0
		if maxReqs > 0 {
			barLen = int(math.Round(float64(b.Requests) / float64(maxReqs) * float64(barMax)))
		}
		bar := strings.Repeat("█", barLen)
		rows = append(rows, fmt.Sprintf("%-*s %s %s",
			labelW, b.Time.Format(timeFmt),
			styleBar.Render(fmt.Sprintf("%-*s", barMax, bar)),
			styleDim.Render(formatComma(b.Requests)),
		))
	}

	return styleBorder.Width(m.width - 4).Render(strings.Join(rows, "\n"))
}

// -- shared panels ------------------------------------------------------------

func (m model) renderPeriodStats(stats *parser.Stats) string {
	return styleBorder.Render(strings.Join([]string{
		styleHeader.Render("Period Stats"),
		"",
		fmt.Sprintf("Requests : %s", styleGood.Render(formatComma(stats.TotalRequests))),
		fmt.Sprintf("Transfer : %s", formatBytes(stats.TotalBytes)),
		fmt.Sprintf("Avg resp : %.1fms", stats.AvgDurationMs),
	}, "\n"))
}

func (m model) renderTopHosts(stats *parser.Stats) string {
	rows := []string{styleHeader.Render("Top Hosts"), ""}
	for _, p := range topN(stats.TopHosts, 8) {
		host := p.key
		if len(host) > 30 {
			host = host[:27] + "..."
		}
		rows = append(rows, fmt.Sprintf("%-30s %s", host, styleGood.Render(formatComma(p.val))))
	}
	if len(stats.TopHosts) == 0 {
		rows = append(rows, styleDim.Render("no data"))
	}
	return styleBorder.Render(strings.Join(rows, "\n"))
}

func (m model) renderTopIPs(stats *parser.Stats) string {
	rows := []string{styleHeader.Render("Top IPs"), ""}
	for _, p := range topN(stats.TopIPs, 8) {
		rows = append(rows, fmt.Sprintf("%-20s %s", p.key, styleGood.Render(formatComma(p.val))))
	}
	if len(stats.TopIPs) == 0 {
		rows = append(rows, styleDim.Render("no data"))
	}
	return styleBorder.Render(strings.Join(rows, "\n"))
}

func (m model) renderStatusCodes(stats *parser.Stats) string {
	groups := map[string]int64{"2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0}
	for code, n := range stats.StatusCodes {
		switch {
		case code >= 200 && code < 300:
			groups["2xx"] += n
		case code >= 300 && code < 400:
			groups["3xx"] += n
		case code >= 400 && code < 500:
			groups["4xx"] += n
		case code >= 500:
			groups["5xx"] += n
		}
	}
	rows := []string{styleHeader.Render("Status Codes"), ""}
	for _, g := range []string{"2xx", "3xx", "4xx", "5xx"} {
		s := formatComma(groups[g])
		var colored string
		switch g {
		case "2xx":
			colored = styleGood.Render(s)
		case "3xx":
			colored = styleDim.Render(s)
		case "4xx":
			colored = styleWarn.Render(s)
		case "5xx":
			colored = styleBad.Render(s)
		}
		rows = append(rows, fmt.Sprintf("%s : %s", g, colored))
	}
	return styleBorder.Render(strings.Join(rows, "\n"))
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

func formatComma(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

type kv struct {
	key string
	val int64
}

func topN(m map[string]int64, n int) []kv {
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].val > pairs[j].val })
	if len(pairs) > n {
		return pairs[:n]
	}
	return pairs
}

func bucketSizeLabel(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return "day"
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// -- entry point --------------------------------------------------------------

func Run(client *sshclient.Client, logPath string) error {
	p := tea.NewProgram(initialModel(client, logPath), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
