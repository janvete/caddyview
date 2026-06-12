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
	tabLive viewTab = 0
	tabDay  viewTab = 1
	tabWeek viewTab = 2
)

var tabLabels = []string{"[1] LIVE", "[2] 1 DAY", "[3] 1 WEEK"}

// tabWindow returns how far back each history tab reaches.
func tabWindow(t viewTab) time.Duration {
	if t == tabWeek {
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// tabBucketSize returns the bucket granularity for the bar chart.
func tabBucketSize(t viewTab) time.Duration {
	if t == tabWeek {
		return 6 * time.Hour
	}
	return time.Hour
}

// -- styles (green phosphor / amber CRT palette) --------------------------------

var (
	colGreen  = lipgloss.Color("40")  // primary phosphor green
	colBright = lipgloss.Color("46")  // highlights
	colDark   = lipgloss.Color("28")  // borders
	colFaint  = lipgloss.Color("22")  // empty bar fill
	colDim    = lipgloss.Color("65")  // secondary text
	colAmber  = lipgloss.Color("214") // warnings
	colRed    = lipgloss.Color("160") // errors

	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colDark).
			Padding(0, 1)

	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(colBright)

	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(colGreen)

	styleTabActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("16")).
			Background(colGreen).
			Padding(0, 1)

	styleTabInactive = lipgloss.NewStyle().
				Foreground(colDim).
				Padding(0, 1)

	styleGood     = lipgloss.NewStyle().Foreground(colGreen)
	styleWarn     = lipgloss.NewStyle().Foreground(colAmber)
	styleBad      = lipgloss.NewStyle().Foreground(colRed)
	styleDim      = lipgloss.NewStyle().Foreground(colDim)
	styleBar      = lipgloss.NewStyle().Foreground(colGreen)
	styleBarEmpty = lipgloss.NewStyle().Foreground(colFaint)
)

// -- history state ------------------------------------------------------------

const historyRefreshInterval = 5 * time.Minute

type historyState struct {
	stats    *parser.Stats
	buckets  []parser.Bucket
	loaded   bool
	loading  bool
	loadedAt time.Time
	err      error
}

// -- messages -----------------------------------------------------------------

type tickMsg time.Time
type newLinesMsg []string
type sysStatsMsg struct {
	cpu      float64
	memUsed  int64
	memTotal int64
}
type historyLoadedMsg struct {
	tab viewTab
	agg *parser.AggregatedHistory
	err error
}
type errMsg struct{ err error }

// -- model --------------------------------------------------------------------

type model struct {
	client  *sshclient.Client
	logPath string
	lineCh  chan string
	doneCh  chan struct{}

	liveStats *parser.Stats
	liveStart time.Time

	activeTab viewTab
	history   [2]*historyState

	cpu      float64
	memUsed  int64
	memTotal int64

	ticks  int
	width  int
	height int
	err    error
}

func initialModel(client *sshclient.Client, logPath string) model {
	m := model{
		client:    client,
		logPath:   logPath,
		lineCh:    make(chan string, 8192),
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

const sysStatsEvery = 5 // seconds; each fetch opens SSH sessions and sleeps 0.5s server-side

func fetchSysStats(client *sshclient.Client) tea.Cmd {
	return func() tea.Msg {
		cpu, used, total, err := client.SysStats()
		if err != nil {
			return sysStatsMsg{}
		}
		return sysStatsMsg{cpu: cpu, memUsed: used, memTotal: total}
	}
}

// drainLines grabs everything currently buffered in the channel as one batch,
// so a traffic burst costs one Update/render cycle instead of one per line.
func drainLines(ch chan string) tea.Cmd {
	return func() tea.Msg {
		var batch []string
		for {
			select {
			case line := <-ch:
				batch = append(batch, line)
				if len(batch) >= 10000 {
					return newLinesMsg(batch)
				}
			default:
				if len(batch) == 0 {
					return nil
				}
				return newLinesMsg(batch)
			}
		}
	}
}

func loadHistory(client *sshclient.Client, logPath string, tab viewTab) tea.Cmd {
	window := tabWindow(tab)
	since := time.Now().Add(-window).Unix()
	maxAgeDays := int(window.Hours()/24) + 1
	return func() tea.Msg {
		agg, err := client.LoadAggregatedHistory(logPath, since, maxAgeDays)
		if err != nil {
			return historyLoadedMsg{tab: tab, err: err}
		}
		agg.FilterSince(since)
		return historyLoadedMsg{tab: tab, agg: agg}
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
		}

	case tickMsg:
		m.ticks++
		cmds := []tea.Cmd{tick(), drainLines(m.lineCh)}
		if m.ticks%sysStatsEvery == 0 {
			cmds = append(cmds, fetchSysStats(m.client))
		}
		if m.activeTab != tabLive {
			if cmd := m.maybeRefreshHistory(m.activeTab); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case newLinesMsg:
		for _, line := range msg {
			if e := parser.ParseLine(line); e != nil {
				m.liveStats.Add(e)
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
		h.loadedAt = time.Now()
		h.err = msg.err
		if msg.err == nil && msg.agg != nil {
			h.stats = msg.agg.ToStats()
			h.buckets = msg.agg.ToBuckets(tabBucketSize(msg.tab))
		}

	case errMsg:
		m.err = msg.err
	}

	return m, nil
}

func (m *model) ensureHistory(tab viewTab) tea.Cmd {
	h := m.history[int(tab)-1]
	if h.loading {
		return nil
	}
	if !h.loaded {
		h.loading = true // first load: show spinner
		return loadHistory(m.client, m.logPath, tab)
	}
	if time.Since(h.loadedAt) > historyRefreshInterval {
		// silent refresh: keep showing old data while new data loads
		return loadHistory(m.client, m.logPath, tab)
	}
	return nil
}

// maybeRefreshHistory triggers a silent background reload for the active history
// tab if its data is older than historyRefreshInterval.
func (m *model) maybeRefreshHistory(tab viewTab) tea.Cmd {
	h := m.history[int(tab)-1]
	if h.loading || !h.loaded {
		return nil
	}
	if time.Since(h.loadedAt) < historyRefreshInterval {
		return nil
	}
	return loadHistory(m.client, m.logPath, tab)
}

// -- view ---------------------------------------------------------------------

func (m model) View() string {
	if m.err != nil {
		return styleBad.Render(fmt.Sprintf("ERROR: %v\n\nPress q to quit.", m.err))
	}
	parts := []string{
		m.renderHeader(),
		m.renderTabBar(),
	}
	switch m.activeTab {
	case tabLive:
		parts = append(parts, m.renderLive())
	case tabDay:
		parts = append(parts, m.renderHistory(0, "LAST 24 HOURS"))
	case tabWeek:
		parts = append(parts, m.renderHistory(1, "LAST 7 DAYS"))
	}
	parts = append(parts, styleDim.Render("  [1] LIVE   [2] 1 DAY   [3] 1 WEEK   [Q] QUIT"))
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
		styleTitle.Render("░▒▓ CADDYVIEW ▓▒░"),
		"  ",
		styleHeader.Render("HOST: ") + m.client.Host(),
		"  ",
		styleHeader.Render("LOG: ") + styleDim.Render(m.logPath),
		"  ",
		styleHeader.Render("UP: ") + time.Since(m.liveStart).Round(time.Second).String(),
		"  ",
		styleHeader.Render("CPU: ") + cpuColor.Render(fmt.Sprintf("%.1f%%", m.cpu)),
		"  ",
		styleHeader.Render("MEM: ") + memColor.Render(fmt.Sprintf("%d/%d MB (%.0f%%)", m.memUsed, m.memTotal, memPct)),
	}, "")
	return lipgloss.NewStyle().
		BorderBottom(true).BorderStyle(lipgloss.DoubleBorder()).
		BorderForeground(colDark).Width(m.width).
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
		BorderForeground(colDark).Width(m.width).
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
		styleHeader.Render("LIVE STATS"),
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
			Render(styleWarn.Render("▒▒ LOADING HISTORY ▒▒"))
	}
	if !h.loaded {
		return lipgloss.NewStyle().Padding(2, 4).Render(styleDim.Render("No data loaded."))
	}
	if h.err != nil {
		return lipgloss.NewStyle().Padding(2, 4).
			Render(styleBad.Render(fmt.Sprintf("ERROR: %v", h.err)))
	}
	if h.stats == nil || h.stats.TotalRequests == 0 {
		return lipgloss.NewStyle().Padding(2, 4).
			Render(styleDim.Render("No log entries found for this period."))
	}

	// Append "updated X ago" to label so the chart title reflects freshness
	age := time.Since(h.loadedAt).Round(time.Second)
	labelWithAge := fmt.Sprintf("%s — updated %s ago", label, age)

	tab := viewTab(idx + 1)
	chart := m.renderBarChart(h.buckets, tabBucketSize(tab), labelWithAge, h.stats)
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

	title := fmt.Sprintf("REQUESTS / %s — %s — TOTAL: %s req, %s",
		bucketSizeLabel(bucketSize), label,
		formatComma(stats.TotalRequests), formatBytes(stats.TotalBytes),
	)

	rows := []string{styleHeader.Render(title), ""}
	for _, b := range sorted {
		barLen := 0
		if maxReqs > 0 {
			barLen = int(math.Round(float64(b.Requests) / float64(maxReqs) * float64(barMax)))
		}
		bar := styleBar.Render(strings.Repeat("▓", barLen)) +
			styleBarEmpty.Render(strings.Repeat("░", barMax-barLen))
		rows = append(rows, fmt.Sprintf("%-*s %s %s",
			labelW, b.Time.Format(timeFmt),
			bar,
			styleDim.Render(formatComma(b.Requests)),
		))
	}

	return styleBorder.Width(m.width - 4).Render(strings.Join(rows, "\n"))
}

// -- shared panels ------------------------------------------------------------

func (m model) renderPeriodStats(stats *parser.Stats) string {
	return styleBorder.Render(strings.Join([]string{
		styleHeader.Render("PERIOD STATS"),
		"",
		fmt.Sprintf("Requests : %s", styleGood.Render(formatComma(stats.TotalRequests))),
		fmt.Sprintf("Transfer : %s", formatBytes(stats.TotalBytes)),
		fmt.Sprintf("Avg resp : %.1fms", stats.AvgDurationMs),
	}, "\n"))
}

func (m model) renderTopHosts(stats *parser.Stats) string {
	rows := []string{styleHeader.Render("TOP HOSTS"), ""}
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
	rows := []string{styleHeader.Render("TOP IPS"), ""}
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
	rows := []string{styleHeader.Render("STATUS CODES"), ""}
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

// topN returns the n highest entries. Ties are broken alphabetically so rows
// with equal counts keep a stable order between renders instead of flickering.
func topN(m map[string]int64, n int) []kv {
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].val != pairs[j].val {
			return pairs[i].val > pairs[j].val
		}
		return pairs[i].key < pairs[j].key
	})
	if len(pairs) > n {
		return pairs[:n]
	}
	return pairs
}

func bucketSizeLabel(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return "DAY"
	case d >= time.Hour:
		return fmt.Sprintf("%dH", int(d.Hours()))
	default:
		return fmt.Sprintf("%dM", int(d.Minutes()))
	}
}

// -- entry point --------------------------------------------------------------

func Run(client *sshclient.Client, logPath string) error {
	p := tea.NewProgram(initialModel(client, logPath), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
