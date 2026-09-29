package dialog

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ---------------------------------------------------------------------------
// costDialog – TUI dialog displaying session cost breakdown
// ---------------------------------------------------------------------------

type costDialog struct {
	BaseDialog

	session    *session.Session
	keyMap     costDialogKeyMap
	scrollview *scrollview.Model

	// cachedLines holds the rendered content for cacheKey. Rebuilding it
	// walks the whole session and restyles every line — far too slow to
	// repeat every frame while scrolling large sessions. The key is a cheap
	// fingerprint (width + item count + tokens/cost) so live updates from a
	// running stream still refresh the view; theme changes clear the cache
	// explicitly since they don't alter the fingerprint.
	cachedLines []string
	cachedKey   costCacheKey
}

// costCacheKey fingerprints the inputs of buildLines. Comparable so a
// per-frame equality check can decide whether the cached lines are current.
type costCacheKey struct {
	width                     int
	themeGeneration           uint64
	itemCount                 int
	inputTokens, outputTokens int64
	totalCost                 float64
}

type costDialogKeyMap struct {
	Close, Copy key.Binding
}

func NewCostDialog(sess *session.Session) Dialog {
	base := BaseDialog{}
	scrollviewView := base.newScrollview(
		scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()),
		scrollview.WithReserveScrollbarSpace(true),
	)
	base.bodyScroll = scrollviewView
	base.bodyFillHeight = true
	return &costDialog{
		BaseDialog: base,
		session:    sess,
		scrollview: scrollviewView,
		keyMap: costDialogKeyMap{
			Close: key.NewBinding(key.WithKeys("esc", "enter", "q")),
			Copy:  key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
		},
	}
}

func (d *costDialog) Init() tea.Cmd { return nil }

func (d *costDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
		d.BlurActions()
	}
	if preparesDialogBody(msg) {
		defer d.renderBody(true)
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if action, handled := d.HandleActionKey(k); handled {
			if action.Code == 0 {
				return d, nil
			}
			msg = action
		}
	}

	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case messages.ThemeChangedMsg:
		d.cachedLines = nil // cached lines carry already-styled ANSI output
		return d, nil
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return d, core.CmdHandler(CloseDialogMsg{})
		case key.Matches(msg, d.keyMap.Copy):
			_ = clipboard.WriteAll(d.renderPlainText())
			return d, notification.SuccessCmd("Cost details copied to clipboard.")
		}
	}
	return d, nil
}

func (d *costDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(70, 50, 120)
	maxHeight = min(d.Height()*70/100, 40)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *costDialog) Position() (row, col int) {
	if d.cardWidth > 0 {
		return CenterPosition(d.Width(), d.Height(), d.cardWidth, d.cardHeight)
	}
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}

func (d *costDialog) View() string { return d.renderBody(false) }
func (d *costDialog) renderBody(prepare bool) string {
	_, maxHeight, contentWidth := d.dialogSize()
	if prepare {
		d.prepareContent(contentWidth)
		return ""
	}
	return d.renderContent(contentWidth, maxHeight)
}

// ---------------------------------------------------------------------------
// totalUsage – per-model / per-message cost record
// ---------------------------------------------------------------------------

type totalUsage struct {
	chat.Usage

	label  string
	model  string // model name (only set for per-message entries)
	cost   float64
	marker bool // true for visual separators (sub-session boundaries)
}

func (u *totalUsage) add(cost float64, usage *chat.Usage) {
	u.cost += cost
	u.Add(usage)
}

func (u *totalUsage) totalInput() int64 {
	return u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
}

func (u *totalUsage) totalTokens() int64 {
	return u.totalInput() + u.OutputTokens
}

func (u *totalUsage) isSubSessionMarker() bool { return u.marker }

// plainTextLine returns a fixed-width plain-text representation used by the
// clipboard-copy output. An optional suffix (e.g. model name) is appended.
func (u *totalUsage) plainTextLine(suffix string) string {
	line := fmt.Sprintf("%-8s  in: %-8s  out: %-8s  %s",
		formatCostPadded(u.cost),
		formatTokenCount(u.totalInput()),
		formatTokenCount(u.OutputTokens),
		u.label)
	if suffix != "" {
		line += "  " + suffix
	}
	return line
}

// ---------------------------------------------------------------------------
// stat – a label/value pair used in the Total section
// ---------------------------------------------------------------------------

type stat struct {
	label string
	value string
}

// ---------------------------------------------------------------------------
// costData – aggregated cost data for a session
// ---------------------------------------------------------------------------

type costData struct {
	total             totalUsage
	agents            []totalUsage
	models            []totalUsage
	messages          []totalUsage
	hasPerMessageData bool
	duration          time.Duration
	messageCount      int
}

// actualMessageCount returns the number of real usage entries, excluding
// sub-session markers and other visual separators.
func (d *costData) actualMessageCount() int {
	n := 0
	for _, m := range d.messages {
		if !m.isSubSessionMarker() {
			n++
		}
	}
	return n
}

// totalStats computes the summary statistics shown in the Total section.
// Both the styled and plain-text renderers consume the same slice.
func (d *costData) totalStats() []stat {
	var stats []stat

	if d.total.ReasoningTokens > 0 {
		stats = append(stats, stat{"reasoning:", formatTokenCount(d.total.ReasoningTokens)})
	}
	if tok := d.total.totalTokens(); tok > 0 && d.total.cost > 0 {
		stats = append(stats, stat{"avg cost/1K tokens:", formatCost(d.total.cost / float64(tok) * 1000)})
	}
	if actual := d.actualMessageCount(); actual > 1 && d.total.cost > 0 {
		stats = append(stats, stat{"avg cost/message:", formatCost(d.total.cost / float64(actual))})
	}
	if candidateIn := d.total.CachedInputTokens + d.total.InputTokens; candidateIn > 0 {
		stats = append(stats, stat{"cache hit rate:", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(d.total.CachedInputTokens)/float64(candidateIn)*100), "0"), ".") + "%"})
	}
	return stats
}

// ---------------------------------------------------------------------------
// Data gathering
// ---------------------------------------------------------------------------

func (d *costDialog) gatherCostData() costData {
	var data costData
	data.duration = d.session.Duration()
	data.messageCount = d.session.MessageCount()
	modelMap := make(map[string]*totalUsage)
	agentMap := make(map[string]*totalUsage)
	msgCounter := 0

	// Aggregation is exact: it sums the per-message records themselves, so it
	// is immune to the double-counting a sum of cumulative session snapshots
	// would produce.
	addAgentUsage := func(label string, cost float64, usage *chat.Usage) {
		if agentMap[label] == nil {
			agentMap[label] = &totalUsage{label: label}
		}
		if usage != nil {
			agentMap[label].add(cost, usage)
			return
		}
		agentMap[label].cost += cost
	}

	addRecord := func(agentName, model string, cost float64, usage *chat.Usage) {
		data.hasPerMessageData = true
		data.total.add(cost, usage)

		model = cmp.Or(model, "unknown")
		if modelMap[model] == nil {
			modelMap[model] = &totalUsage{label: model}
		}
		modelMap[model].add(cost, usage)

		// Records without an agent name stay honestly unattributed instead of
		// being assigned to some agent arbitrarily.
		addAgentUsage(cmp.Or(agentName, "(unattributed)"), cost, usage)

		msgCounter++
		msgLabel := fmt.Sprintf("#%d", msgCounter)
		if agentName != "" {
			msgLabel = fmt.Sprintf("#%d [%s]", msgCounter, agentName)
		}
		data.messages = append(data.messages, totalUsage{
			label: msgLabel,
			model: model,
			cost:  cost,
			Usage: *usage,
		})
	}

	addCompactionCost := func(item *session.Item) {
		data.hasPerMessageData = true
		usage := item.Usage
		if usage == nil {
			usage = &chat.Usage{}
		}
		data.total.add(item.Cost, usage)
		// Summary items carry no agent attribution; keep compaction spend in
		// its own By Agent bucket rather than crediting an agent for it.
		addAgentUsage("compaction", item.Cost, usage)
		// The summary may have been generated by a dedicated compaction model;
		// attribute the spend to it so By Model stays honest. Items persisted
		// before the model was recorded stay out of the model breakdown.
		if item.Model != "" {
			if modelMap[item.Model] == nil {
				modelMap[item.Model] = &totalUsage{label: item.Model}
			}
			modelMap[item.Model].add(item.Cost, usage)
		}
		data.messages = append(data.messages, totalUsage{label: "compaction", model: item.Model, cost: item.Cost, Usage: *usage})
	}

	addMarker := func(label string) {
		data.messages = append(data.messages, totalUsage{label: label, marker: true})
	}

	var walkSession func(sess *session.Session)
	walkSession = func(sess *session.Session) {
		// Iterate a locked snapshot: runtime goroutines append to the live
		// Messages slice while the dialog renders. The snapshot releases
		// sess.mu before the recursive sub-session accessors take theirs.
		for _, item := range sess.MessagesSnapshot() {
			switch {
			case item.IsMessage():
				msg := item.Message
				if msg.Message.Role != chat.MessageRoleSystem && msg.Message.Usage != nil {
					addRecord(msg.AgentName, msg.Message.Model, msg.Message.Cost, msg.Message.Usage)
				}
			case item.IsSubSession():
				addMarker("── sub-session start ──")
				walkSession(item.SubSession)
				if subCost := item.SubSession.TotalCost(); subCost > 0 {
					addMarker(fmt.Sprintf("── sub-session end (%s) ──", formatCost(subCost)))
				} else {
					addMarker("── sub-session end ──")
				}
			}
			if item.Summary != "" && (item.Cost > 0 || item.Usage != nil) {
				addCompactionCost(&item)
			}
		}
	}

	walkSession(d.session)

	// Fall back to remote mode if no per-message data was found.
	if !data.hasPerMessageData {
		for _, record := range d.session.MessageUsageHistorySnapshot() {
			addRecord(record.AgentName, record.Model, record.Cost, &record.Usage)
		}
	}

	for _, m := range modelMap {
		data.models = append(data.models, *m)
	}
	slices.SortFunc(data.models, func(a, b totalUsage) int {
		return cmp.Compare(b.cost, a.cost)
	})

	for _, a := range agentMap {
		data.agents = append(data.agents, *a)
	}
	slices.SortFunc(data.agents, func(a, b totalUsage) int {
		return cmp.Or(cmp.Compare(b.cost, a.cost), cmp.Compare(a.label, b.label))
	})

	// Fall back to session-level totals (e.g. past sessions without per-message data).
	if !data.hasPerMessageData {
		inputTokens, outputTokens := d.session.Usage()
		data.total = totalUsage{
			cost: d.session.TotalCost(),
			Usage: chat.Usage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}
	}

	return data
}

// ---------------------------------------------------------------------------
// Styled rendering (TUI view)
// ---------------------------------------------------------------------------

func (d *costDialog) renderContent(contentWidth, maxHeight int) string {
	if k := d.cacheKey(contentWidth); d.cachedLines == nil || d.cachedKey != k {
		d.cachedLines = d.buildLines(contentWidth)
		d.cachedKey = k
	}
	return d.applyScrolling(d.cachedLines, contentWidth, maxHeight)
}

// cacheKey snapshots the session state that buildLines depends on. TotalCost
// walks all items (O(n) without rendering, a few µs even on huge sessions)
// and catches cost changes that don't touch the parent's counters, e.g. a
// live sub-session or a compaction item.
func (d *costDialog) cacheKey(contentWidth int) costCacheKey {
	input, output := d.session.Usage()
	return costCacheKey{
		width:           contentWidth,
		themeGeneration: styles.ThemeGeneration(),
		itemCount:       d.session.ItemCount(),
		inputTokens:     input,
		outputTokens:    output,
		totalCost:       d.session.TotalCost(),
	}
}

func (d *costDialog) buildLines(contentWidth int) []string {
	data := d.gatherCostData()

	// Header
	header := RenderTitle("Session Cost Details", contentWidth, styles.DialogTitleStyle)
	if meta := d.headerMeta(data); meta != "" {
		header += "\n" + styles.DialogOptionsStyle.Width(contentWidth).Render(meta)
	}

	lines := []string{
		header,
		RenderSeparator(contentWidth),
		"",
		sectionStyle().Render("Total") + "  " + accentStyle().Render(formatCost(data.total.cost)),
		"",
	}
	totalStats := append([]stat{
		{label: "tokens:", value: formatTokenCount(data.total.totalTokens())},
		{label: "in:", value: inputValue(data.total, true)},
		{label: "out:", value: formatTokenCount(data.total.OutputTokens)},
	}, data.totalStats()...)
	labelWidth := statLabelWidth(totalStats)
	for _, s := range totalStats {
		lines = append(lines, styledStat(s, labelWidth))
	}
	lines = append(lines, "")

	// By Agent
	if len(data.agents) > 0 {
		lines = append(lines, sectionStyle().Render("By Agent"), "")
		labelWidth := usageLabelWidth(data.agents)
		for _, a := range data.agents {
			lines = append(lines, d.renderUsageLine(a, data.total.cost, labelWidth, false))
		}
		lines = append(lines, "")
	}

	// By Model
	if len(data.models) > 0 {
		lines = append(lines, sectionStyle().Render("By Model"), "")
		labelWidth := usageLabelWidth(data.models)
		for _, m := range data.models {
			lines = append(lines, d.renderUsageLine(m, data.total.cost, labelWidth, false))
		}
		lines = append(lines, "")
	}

	// By Message
	if len(data.messages) > 0 {
		lines = append(lines, sectionStyle().Render("By Message"), "")
		labelWidth := usageLabelWidth(data.messages)
		for _, m := range data.messages {
			if m.isSubSessionMarker() {
				lines = append(lines, styles.MutedStyle.Render(m.label))
			} else {
				lines = append(lines, d.renderUsageLine(m, data.total.cost, labelWidth, true))
			}
		}
		lines = append(lines, "")
	} else if !data.hasPerMessageData && data.total.cost > 0 {
		lines = append(lines, styles.MutedStyle.Render("Per-message breakdown not available for this session."), "")
	}

	return lines
}

// headerMeta returns the duration/message-count line, or "" if empty.
func (d *costDialog) headerMeta(data costData) string {
	var parts []string
	if data.duration > 0 {
		parts = append(parts, "duration: "+formatDuration(data.duration))
	}
	if data.messageCount > 0 {
		parts = append(parts, fmt.Sprintf("messages: %d", data.messageCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "  •  ")
}

func statLabelWidth(stats []stat) int {
	width := 0
	for _, s := range stats {
		width = max(width, lipgloss.Width(s.label))
	}
	return width
}

func styledStat(s stat, labelWidth int) string {
	return fmt.Sprintf("%s %s", labelStyle().Render(padToWidth(s.label, labelWidth)), valueStyle().Render(s.value))
}

func inputValue(u totalUsage, showBreakdown bool) string {
	value := formatTokenCount(u.totalInput())
	if showBreakdown && (u.CachedInputTokens > 0 || u.CacheWriteTokens > 0) {
		value += fmt.Sprintf(" (%s new + %s cached + %s cache write)",
			formatTokenCount(u.InputTokens),
			formatTokenCount(u.CachedInputTokens),
			formatTokenCount(u.CacheWriteTokens))
	}
	return value
}

func usageLabelWidth(usages []totalUsage) int {
	width := 0
	for _, usage := range usages {
		if !usage.isSubSessionMarker() {
			width = max(width, lipgloss.Width(usage.label))
		}
	}
	return width
}

func (d *costDialog) renderUsageLine(u totalUsage, totalCost float64, labelWidth int, byMessage bool) string {
	percentage := ""
	percentageValue := 0.0
	if totalCost > 0 && u.cost > 0 {
		if pct := u.cost / totalCost * 100; pct >= 0.1 {
			percentage = fmt.Sprintf("%.0f%%", pct)
			percentageValue = pct
		}
	}

	showCacheStatus := u.CachedInputTokens > 0 || (byMessage && u.model != "")
	var extras []string
	if percentage != "" || (totalCost > 0 && showCacheStatus) {
		percentageStyle := valueStyle()
		if byMessage && percentage != "" {
			percentageStyle = costPercentageStyle(percentageValue)
		}
		extras = append(extras, percentageStyle.Render(fmt.Sprintf("%-4s", percentage)))
	}
	if u.CachedInputTokens > 0 {
		extras = append(extras, valueStyle().Render("cached: "+formatTokenCount(u.CachedInputTokens)))
	} else if showCacheStatus {
		extras = append(extras, styles.WarningStyle.Render("cache miss"))
	}

	suffix := ""
	if len(extras) > 0 {
		suffix = "  " + strings.Join(extras, "  ")
	}
	return fmt.Sprintf("%s  %s %s  %s %s  %s%s",
		accentStyle().Render(padRight(formatCostPadded(u.cost))),
		labelStyle().Render("in:"),
		valueStyle().Render(padRight(formatTokenCount(u.totalInput()))),
		labelStyle().Render("out:"),
		valueStyle().Render(padRight(formatTokenCount(u.OutputTokens))),
		accentStyle().Render(padToWidth(u.label, labelWidth)),
		suffix)
}

func (d *costDialog) applyScrolling(allLines []string, contentWidth, _ int) string {
	width, _, _ := d.dialogSize()
	footer := d.RenderActionKeys(contentWidth+d.scrollview.ReservedCols(), "c", "Copy")
	return d.RenderScrollableBody(styles.DialogStyle, width, strings.Join(allLines[:3], "\n"), strings.Join(allLines[3:], "\n"), footer)
}

// ---------------------------------------------------------------------------
// Plain-text rendering (clipboard copy)
// ---------------------------------------------------------------------------

func (d *costDialog) renderPlainText() string {
	data := d.gatherCostData()
	var lines []string

	// Total section
	inputLine := "in: " + formatTokenCount(data.total.totalInput())
	if data.total.CachedInputTokens > 0 || data.total.CacheWriteTokens > 0 {
		inputLine += fmt.Sprintf(" (%s new + %s cached + %s cache write)",
			formatTokenCount(data.total.InputTokens),
			formatTokenCount(data.total.CachedInputTokens),
			formatTokenCount(data.total.CacheWriteTokens))
	}

	lines = append(lines, "Session Cost Details", "", "Total  "+formatCost(data.total.cost), "",
		"tokens: "+formatTokenCount(data.total.totalTokens()),
		inputLine, "out: "+formatTokenCount(data.total.OutputTokens))

	for _, s := range data.totalStats() {
		lines = append(lines, s.label+" "+s.value)
	}
	lines = append(lines, "")

	// By Agent
	if len(data.agents) > 0 {
		lines = append(lines, "By Agent")
		for _, a := range data.agents {
			lines = append(lines, a.plainTextLine(""))
		}
		lines = append(lines, "")
	}

	// By Model
	if len(data.models) > 0 {
		lines = append(lines, "By Model")
		for _, m := range data.models {
			suffix := ""
			if m.CachedInputTokens > 0 {
				suffix = "cached: " + formatTokenCount(m.CachedInputTokens)
			}
			lines = append(lines, m.plainTextLine(suffix))
		}
		lines = append(lines, "")
	}

	// By Message
	if len(data.messages) > 0 {
		lines = append(lines, "By Message")
		for _, m := range data.messages {
			if m.isSubSessionMarker() {
				lines = append(lines, m.label)
			} else {
				suffix := ""
				if m.model != "" {
					suffix = "(" + m.model + ")"
				}
				lines = append(lines, m.plainTextLine(suffix))
			}
		}
	}

	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Style helpers (functions so they pick up theme changes dynamically)
// ---------------------------------------------------------------------------

func sectionStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(styles.TextSecondary)
}

func labelStyle() lipgloss.Style { return lipgloss.NewStyle().Bold(true) }

func valueStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(styles.TextSecondary)
}

func costPercentageStyle(percentage float64) lipgloss.Style {
	const warningAt = 0.35

	ratio := min(max(percentage/100, 0), 1)
	from, to := styles.TextSecondary, styles.Warning
	if ratio >= warningAt {
		from, to = styles.Warning, styles.Error
		ratio = (ratio - warningAt) / (1 - warningAt)
	} else {
		ratio /= warningAt
	}

	fromR, fromG, fromB := styles.ColorToRGB(from)
	toR, toG, toB := styles.ColorToRGB(to)
	return lipgloss.NewStyle().Foreground(styles.RGBToColor(
		fromR+(toR-fromR)*ratio,
		fromG+(toG-fromG)*ratio,
		fromB+(toB-fromB)*ratio,
	))
}

func accentStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(styles.Highlight)
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func formatCost(cost float64) string {
	return toolcommon.FormatCostPrecise(cost)
}

func formatCostPadded(cost float64) string {
	if cost < 0 {
		return "-" + formatCostPadded(-cost)
	}
	if cost < 0.0001 {
		return "$0.0000"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f  ", cost)
}

func formatTokenCount(count int64) string {
	return toolcommon.FormatTokenCount(count)
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func padRight(s string) string {
	return padToWidth(s, 8)
}

func padToWidth(s string, width int) string {
	if currentWidth := lipgloss.Width(s); currentWidth < width {
		return s + strings.Repeat(" ", width-currentWidth)
	}
	return s
}

func (d *costDialog) prepareContent(contentWidth int) {
	if k := d.cacheKey(contentWidth); d.cachedLines == nil || d.cachedKey != k {
		d.cachedLines = d.buildLines(contentWidth)
		d.cachedKey = k
	}
	width, _, _ := d.dialogSize()
	allLines := d.cachedLines
	footer := d.RenderActionKeys(contentWidth+d.scrollview.ReservedCols(), "c", "Copy")
	d.PrepareScrollableBody(styles.DialogStyle, width, strings.Join(allLines[:3], "\n"), strings.Join(allLines[3:], "\n"), footer)
}

func (d *costDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.renderBody(true)
	return cmd
}
