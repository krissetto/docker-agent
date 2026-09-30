package dialog

import (
	"fmt"
	"image/color"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ---------------------------------------------------------------------------
// contextDialog – TUI dialog displaying the context-window composition
// ---------------------------------------------------------------------------

type contextDialog struct {
	BaseDialog

	breakdown    *runtime.ContextBreakdown
	liveSessions []runtime.LiveSession
	keyMap       contextDialogKeyMap
	scrollview   *scrollview.Model

	// selected indexes the combined selectable rows: live sessions first
	// (0..len(liveSessions)-1), then breakdown.AttachedFiles; -1 when there
	// is nothing to select.
	selected int
	// rowLines maps each selectable row (same order as the selection index)
	// to its line index in the scrollable content region. Rebuilt on every
	// render and used to keep the selection visible while navigating.
	rowLines []int
}

type contextDialogKeyMap struct {
	Close, Copy, Up, Down, Drop, Compact key.Binding
}

// NewContextDialog creates the /context dialog showing the estimated
// context-window composition by category, the live-session team view
// (explicitly compactable), plus the per-file inventory of attached files
// (droppable) and prompt files.
func NewContextDialog(breakdown *runtime.ContextBreakdown, liveSessions ...runtime.LiveSession) Dialog {
	if breakdown == nil {
		breakdown = &runtime.ContextBreakdown{}
	}
	base := BaseDialog{}
	view := base.newScrollview(scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()), scrollview.WithReserveScrollbarSpace(true))
	base.bodyScroll = view
	d := &contextDialog{
		BaseDialog:   base,
		breakdown:    breakdown,
		liveSessions: liveSessions,
		selected:     -1,
		scrollview:   view,
		keyMap: contextDialogKeyMap{
			Close:   key.NewBinding(key.WithKeys("esc", "q")),
			Copy:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
			Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑", "select")),
			Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓", "select")),
			Drop:    key.NewBinding(key.WithKeys("d", "x", "backspace", "delete"), key.WithHelp("d", "drop")),
			Compact: key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "compact")),
		},
	}
	if d.selectableCount() > 0 {
		d.selected = 0
	}
	return d
}

// selectableCount returns the number of selectable rows: live sessions plus
// attached files.
func (d *contextDialog) selectableCount() int {
	return len(d.liveSessions) + len(d.breakdown.AttachedFiles)
}

// selectedLiveSession returns the live-session row under the selection.
func (d *contextDialog) selectedLiveSession() (runtime.LiveSession, bool) {
	if d.selected >= 0 && d.selected < len(d.liveSessions) {
		return d.liveSessions[d.selected], true
	}
	return runtime.LiveSession{}, false
}

// selectedAttachedIndex returns the breakdown.AttachedFiles index under the
// selection.
func (d *contextDialog) selectedAttachedIndex() (int, bool) {
	idx := d.selected - len(d.liveSessions)
	if d.selected >= len(d.liveSessions) && idx >= 0 && idx < len(d.breakdown.AttachedFiles) {
		return idx, true
	}
	return -1, false
}

func (d *contextDialog) Init() tea.Cmd { return nil }

func (d *contextDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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

	if m, ok := msg.(tea.MouseClickMsg); ok && m.Button == tea.MouseLeft {
		x, y, w, h := d.BodyScrollBounds()
		if m.X >= x && m.X < x+w && m.Y >= y && m.Y < y+h {
			line := m.Y - y + d.BodyScrollOffset()
			for i, at := range d.rowLines {
				if line == at {
					d.selected = i
					return d, nil
				}
			}
		}
	}

	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		if cmd, handled := d.handleKey(keyMsg); handled {
			return d, cmd
		}
	}
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}
	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	}
	return d, nil
}

// handleKey processes dialog-level keys. Up/Down are claimed for selection
// only while selectable rows (live sessions, attached files) are listed;
// without them the keys fall through to the scrollview and keep their plain
// scrolling behavior.
func (d *contextDialog) handleKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	selectable := d.selectableCount() > 0
	switch {
	case key.Matches(msg, d.keyMap.Close):
		return core.CmdHandler(CloseDialogMsg{}), true
	case key.Matches(msg, d.keyMap.Copy):
		_ = clipboard.WriteAll(d.renderPlainText())
		return notification.SuccessCmd("Context breakdown copied to clipboard."), true
	case key.Matches(msg, d.keyMap.Up) && selectable:
		d.moveSelection(-1)
		return nil, true
	case key.Matches(msg, d.keyMap.Down) && selectable:
		d.moveSelection(1)
		return nil, true
	case key.Matches(msg, d.keyMap.Compact) && len(d.liveSessions) > 0:
		return d.compactSelected(), true
	case key.Matches(msg, d.keyMap.Drop):
		if idx, ok := d.selectedAttachedIndex(); ok {
			return d.dropSelected(idx), true
		}
	}
	return nil, false
}

// moveSelection moves the selection by delta across the combined rows (live
// sessions, then attached files), clamped to the list bounds, and scrolls
// the selected row into view.
func (d *contextDialog) moveSelection(delta int) {
	n := d.selectableCount()
	if n == 0 {
		return
	}
	d.selected = min(max(d.selected+delta, 0), n-1)
	if d.selected < len(d.rowLines) {
		d.scrollview.EnsureLineVisible(d.rowLines[d.selected])
	}
}

// compactSelected requests an explicit compaction of the selected live
// session and closes the dialog. The row's SessionID and AgentName travel on
// the message so the handler can route the current root session through the
// classic /compact path and live sub-agent sessions through the targeted
// runtime API.
func (d *contextDialog) compactSelected() tea.Cmd {
	row, ok := d.selectedLiveSession()
	if !ok {
		return nil
	}
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(messages.CompactSessionMsg{SessionID: row.SessionID, AgentName: row.AgentName}),
	)
}

// dropSelected removes the attached file at idx from the local inventory
// and asks the app to drop it from the session. The local removal is
// optimistic, mirroring the session-browser delete flow; the handler reports
// success or failure through a notification.
func (d *contextDialog) dropSelected(idx int) tea.Cmd {
	files := d.breakdown.AttachedFiles
	path := files[idx].Path
	d.breakdown.AttachedFiles = slices.Delete(files, idx, idx+1)
	if n := d.selectableCount(); d.selected >= n {
		d.selected = n - 1
	}
	return core.CmdHandler(messages.DropAttachedFileMsg{Path: path})
}

func (d *contextDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(70, 50, 100)
	maxHeight = min(d.Height()*70/100, 30)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *contextDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *contextDialog) View() string { return d.renderBody(false) }
func (d *contextDialog) renderBody(prepare bool) string {
	_, maxHeight, contentWidth := d.dialogSize()
	if prepare {
		d.prepareContent(contentWidth, maxHeight)
		return ""
	}
	return d.renderContent(contentWidth, maxHeight)
}

// ---------------------------------------------------------------------------
// Row model – one renderable category
// ---------------------------------------------------------------------------

// contextRow is one line of the breakdown: a category (or the free-space
// remainder) with its estimated token count and item count.
type contextRow struct {
	label  string
	tokens int64
	items  int
	noun   string // singular item noun ("message", "tool", "file", "result")
	free   bool   // true for the free-space remainder row
}

// contextRows flattens the breakdown into display order. Categories are
// always listed (zero values included) so users see every bucket the
// runtime accounts for; the free-space row is appended only when the
// context limit is known and not already exceeded by the estimate.
func contextRows(b *runtime.ContextBreakdown) []contextRow {
	rows := []contextRow{
		{label: "System prompt", tokens: b.SystemPrompt.Tokens, items: b.SystemPrompt.Items, noun: "message"},
		{label: "Tool definitions", tokens: b.ToolDefinitions.Tokens, items: b.ToolDefinitions.Items, noun: "tool"},
		{label: "Prompt files", tokens: b.PromptFiles.Tokens, items: b.PromptFiles.Items, noun: "file"},
		{label: "Messages", tokens: b.Messages.Tokens, items: b.Messages.Items, noun: "message"},
		{label: "Tool results", tokens: b.ToolResults.Tokens, items: b.ToolResults.Items, noun: "result"},
		{label: "Compaction summary", tokens: b.CompactionSummary.Tokens, items: b.CompactionSummary.Items, noun: "summary"},
	}
	if free := b.ContextLimit - b.TotalTokens(); b.ContextLimit > 0 && free > 0 {
		rows = append(rows, contextRow{label: "Free space", tokens: free, free: true})
	}
	return rows
}

// itemsSuffix returns the parenthesized item count, e.g. "(12 messages)".
func (r *contextRow) itemsSuffix() string {
	if r.free || r.items == 0 {
		return ""
	}
	noun := r.noun
	if r.items > 1 {
		noun += "s"
	}
	return fmt.Sprintf("(%d %s)", r.items, noun)
}

// scaleTokens returns the denominator percentages and the usage bar are
// computed against: the context limit when known, otherwise the estimated
// total (the bar then shows relative composition instead of fill level).
func scaleTokens(b *runtime.ContextBreakdown) int64 {
	if b.ContextLimit > 0 {
		return max(b.ContextLimit, b.TotalTokens())
	}
	return b.TotalTokens()
}

// percentLabel formats tokens as a percentage of scale: "<1%" for tiny
// non-zero slices, "-" for empty ones.
func percentLabel(tokens, scale int64) string {
	if tokens <= 0 || scale <= 0 {
		return "-"
	}
	pct := float64(tokens) / float64(scale) * 100
	if pct < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// ---------------------------------------------------------------------------
// Styled rendering (TUI view)
// ---------------------------------------------------------------------------

// contextBarGlyphs are the block glyphs of the stacked usage bar.
const (
	contextBarFilled = "█"
	contextBarFree   = "░"
	contextRowMarker = "■"
)

// contextHeaderLines is the number of fixed (non-scrolling) ENTRIES at the
// top of the lines array: the header block (title, plus its meta line(s))
// + separator + spacer. It is an array-index count, not a rendered-row
// count: the header entry's own visual height varies (an extra warning line
// renders when a dedicated compaction model caps the limit), so callers
// that need the fixed block's rendered height measure it directly (see
// applyScrolling) instead of assuming one row per entry.
const contextHeaderLines = 3

// contextEstimateNote labels every figure in the dialog as an estimate, as
// the counts come from a heuristic rather than the provider's tokenizer.
const contextEstimateNote = "Token counts are estimates; the provider's tokenizer may count differently."

// contextDropNote explains the scope of dropping an attachment: the file
// stops being shared forward, but past messages keep their inlined copy.
const contextDropNote = "Dropping a file removes it from the session's attachment list (sub-agents and skills stop receiving it); content already inlined in past messages stays until compaction."

// contextCompactNote explains the live-session rows: Enter queues an
// explicit compaction that executes on the selected session's own run loop
// at a safe point, so it never interrupts an in-flight model turn.
const contextCompactNote = "Press Enter to compact the selected live session; sub-agent compactions run at the session's next safe point."

// contextSummarySection titles the verbatim latest-compaction-summary text
// in both the styled view and the plain-text copy.
const contextSummarySection = "Latest compaction summary"

// categoryColors returns the per-category accent colors, aligned with the
// order of contextRows. Hues are used categorically (Error's rose tint
// carries no alarm semantics here); each maps to a distinct color in the
// bundled themes. A function (not a var) so it picks up theme changes.
func categoryColors() []color.Color {
	return []color.Color{
		styles.MobyBlue,    // system prompt
		styles.Info,        // tool definitions
		styles.BadgePurple, // prompt files
		styles.Success,     // messages
		styles.Warning,     // tool results
		styles.Error,       // compaction summary
	}
}

func (d *contextDialog) renderContent(contentWidth, maxHeight int) string {
	local := *d
	local.rowLines = nil
	return local.renderContextContent(contentWidth, maxHeight, false)
}

func (d *contextDialog) prepareContent(contentWidth, maxHeight int) {
	d.renderContextContent(contentWidth, maxHeight, true)
}

func (d *contextDialog) renderContextContent(contentWidth, maxHeight int, prepare bool) string {
	b := d.breakdown
	rows := contextRows(b)
	scale := scaleTokens(b)

	header := RenderTitle("Context Window", contentWidth, styles.DialogTitleStyle)
	if meta := contextHeaderMeta(b); meta != "" {
		header += "\n" + styles.DialogOptionsStyle.Width(contentWidth).Render(meta)
	}
	if compactionMeta := contextCompactionMeta(b); compactionMeta != "" {
		header += "\n" + styles.WarningStyle.Width(contentWidth).Render(compactionMeta)
	}

	lines := []string{
		header,
		RenderSeparator(contentWidth),
		"",
		renderContextBar(rows, scale, contentWidth),
		styles.MutedStyle.Render(usageSummary(b)),
		"",
	}

	labelWidth := contextLabelWidth(rows)
	colors := categoryColors()
	for i, row := range rows {
		lines = append(lines, renderContextRow(&row, scale, labelWidth, markerColor(i, row, colors)))
	}

	d.rowLines = d.rowLines[:0]
	lines = d.appendLiveSessions(lines)
	lines = d.appendInventory(lines, scale, contentWidth)
	lines = d.appendCompactionSummary(lines, contentWidth)

	lines = append(lines, "")
	lines = append(lines, wrapMutedLines(contextEstimateNote, contentWidth)...)
	if len(d.liveSessions) > 0 {
		lines = append(lines, wrapMutedLines(contextCompactNote, contentWidth)...)
	}
	if len(b.AttachedFiles) > 0 {
		lines = append(lines, wrapMutedLines(contextDropNote, contentWidth)...)
	}

	if prepare {
		width, _, _ := d.dialogSize()
		footer := d.RenderPickerFooter(contentWidth+d.scrollview.ReservedCols(), actionsForKeys(d.helpKeys()...)...)
		d.PrepareScrollableBody(styles.DialogStyle, width, strings.Join(lines[:contextHeaderLines], "\n"), strings.Join(lines[contextHeaderLines:], "\n"), footer)
		return ""
	}
	return d.applyScrolling(lines, contentWidth, maxHeight)
}

// appendLiveSessions renders the team view: one selectable row per live
// session (the current root first, then every running sub-agent session)
// with its agent identity, short session ID and context budget. Omitted when
// the runtime does not expose live-session tracking.
func (d *contextDialog) appendLiveSessions(lines []string) []string {
	if len(d.liveSessions) == 0 {
		return lines
	}
	lines = append(lines, "", sectionStyle().Render("Live sessions"))
	labelWidth := liveAgentLabelWidth(d.liveSessions)
	for i := range d.liveSessions {
		d.rowLines = append(d.rowLines, len(lines)-contextHeaderLines)
		lines = append(lines, renderLiveSessionRow(&d.liveSessions[i], labelWidth, i == d.selected))
	}
	return lines
}

// liveAgentLabelWidth returns the display width of the agent-name column,
// clamped so one long name cannot push the budget columns off screen.
func liveAgentLabelWidth(rows []runtime.LiveSession) int {
	width := 0
	for i := range rows {
		width = max(width, lipgloss.Width(rows[i].AgentName))
	}
	return min(max(width, 4), 24)
}

// renderLiveSessionRow renders one team row:
// "▶ developer  0f9e8d7c  55.1K of 200.0K (28%)".
func renderLiveSessionRow(row *runtime.LiveSession, labelWidth int, selected bool) string {
	prefix := "  "
	nameStyle := labelStyle()
	if selected {
		prefix = accentStyle().Render("▶ ")
		nameStyle = nameStyle.Foreground(styles.Highlight)
	}
	name := truncateName(row.AgentName, labelWidth)
	pad := strings.Repeat(" ", max(0, labelWidth-lipgloss.Width(name)))
	line := fmt.Sprintf("%s%s%s  %s  %s",
		prefix,
		nameStyle.Render(name),
		pad,
		styles.MutedStyle.Render(row.ShortID()),
		valueStyle().Render(liveSessionBudget(row)))
	if row.Current {
		line += "  " + styles.MutedStyle.Render("(current)")
	}
	return line
}

// liveSessionBudget formats a live session's context budget: used tokens,
// window and percentage, or an explicit unknown-limit reading.
func liveSessionBudget(row *runtime.LiveSession) string {
	used := row.UsedTokens()
	if row.ContextLimit <= 0 {
		return formatTokenCount(used) + " tokens, limit unknown"
	}
	return fmt.Sprintf("%s of %s (%s)",
		formatTokenCount(used),
		formatTokenCount(row.ContextLimit),
		percentLabel(used, row.ContextLimit))
}

// appendInventory renders the per-file inventory under the category rows:
// the session's attached files (selectable, droppable) and the resolved
// prompt files (config-driven, read-only). Empty sections are omitted. The
// content-region line index of every attached row is recorded so navigation
// can keep the selection visible.
func (d *contextDialog) appendInventory(lines []string, scale int64, contentWidth int) []string {
	b := d.breakdown
	labelWidth := fileLabelWidth(b)

	if len(b.AttachedFiles) > 0 {
		lines = append(lines, "", sectionStyle().Render("Attached files"))
		for i := range b.AttachedFiles {
			d.rowLines = append(d.rowLines, len(lines)-contextHeaderLines)
			lines = append(lines, renderContextFileRow(&b.AttachedFiles[i], scale, labelWidth, contentWidth, len(d.liveSessions)+i == d.selected))
		}
	}
	if len(b.PromptFileItems) > 0 {
		lines = append(lines, "", sectionStyle().Render("Prompt files"))
		for i := range b.PromptFileItems {
			lines = append(lines, renderContextFileRow(&b.PromptFileItems[i], scale, labelWidth, contentWidth, false))
		}
	}
	return lines
}

// appendCompactionSummary renders the verbatim text of the latest
// compaction summary, wrapped to the content width. Placed after the
// selectable sections so the rowLines mapping built by appendLiveSessions/
// appendInventory stays aligned with the selection indexes. Omitted
// entirely (no ghost title) when the session has never been compacted.
func (d *contextDialog) appendCompactionSummary(lines []string, contentWidth int) []string {
	summary := d.breakdown.LatestCompactionSummary
	if summary == "" {
		return lines
	}
	lines = append(lines, "", sectionStyle().Render(contextSummarySection))
	return append(lines, wrapTextLines(summary, contentWidth)...)
}

// fileLabelWidth returns the display width of the inventory file-name
// column, sized to the longest base name across both sections and clamped
// so one long name cannot push the token columns off screen.
func fileLabelWidth(b *runtime.ContextBreakdown) int {
	width := 0
	for _, files := range [][]runtime.ContextFile{b.AttachedFiles, b.PromptFileItems} {
		for i := range files {
			width = max(width, lipgloss.Width(filepath.Base(files[i].Path)))
		}
	}
	return min(max(width, 4), 24)
}

// truncateName shortens name to maxWidth display cells, ellipsizing the end.
func truncateName(name string, maxWidth int) string {
	if lipgloss.Width(name) <= maxWidth {
		return name
	}
	r := []rune(name)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > maxWidth {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// renderContextFileRow renders one inventory line:
// "▶ main.go   2.1K   2%  ~/proj/main.go".
func renderContextFileRow(file *runtime.ContextFile, scale int64, labelWidth, contentWidth int, selected bool) string {
	prefix := "  "
	nameStyle := labelStyle()
	if selected {
		prefix = accentStyle().Render("▶ ")
		nameStyle = nameStyle.Foreground(styles.Highlight)
	}
	name := truncateName(filepath.Base(file.Path), labelWidth)
	pad := strings.Repeat(" ", max(0, labelWidth-lipgloss.Width(name)))

	line := fmt.Sprintf("%s%s%s  %s  %s",
		prefix,
		nameStyle.Render(name),
		pad,
		valueStyle().Render(padRight(fileTokensLabel(file))),
		valueStyle().Render(fmt.Sprintf("%4s", percentLabel(file.Tokens, scale))))

	if suffix := filePathSuffix(file, contentWidth-lipgloss.Width(line)-2); suffix != "" {
		line += "  " + suffix
	}
	return line
}

// fileTokensLabel formats a file's token estimate, "-" when it contributes
// nothing inline (missing, binary-unsupported, or oversized files).
func fileTokensLabel(file *runtime.ContextFile) string {
	if file.Tokens <= 0 {
		return "-"
	}
	return formatTokenCount(file.Tokens)
}

// filePathSuffix renders the muted, home-shortened path (plus a missing
// marker) that trails a file row, truncated to the available width. The
// path is omitted when there is no room for a meaningful fragment.
func filePathSuffix(file *runtime.ContextFile, available int) string {
	const missingMark = "(missing)"
	var parts []string
	if file.Missing {
		available -= lipgloss.Width(missingMark) + 1
	}
	if available >= 8 {
		parts = append(parts, styles.MutedStyle.Render(truncatePath(pathx.ShortenHome(file.Path), available)))
	}
	if file.Missing {
		parts = append(parts, styles.ErrorStyle.Render(missingMark))
	}
	return strings.Join(parts, " ")
}

// wrapMutedLines wraps text to width in the muted style and returns the
// individual lines, so the scrollview's line accounting stays exact.
func wrapMutedLines(text string, width int) []string {
	return strings.Split(styles.MutedStyle.Width(width).Render(text), "\n")
}

// wrapTextLines wraps text to width in the default style, preserving hard
// newlines, and returns the individual lines so the scrollview's line
// accounting stays exact.
func wrapTextLines(text string, width int) []string {
	return strings.Split(lipgloss.NewStyle().Width(width).Render(text), "\n")
}

// markerColor picks the row's accent color: its category color, or muted
// for the free-space remainder.
func markerColor(i int, row contextRow, colors []color.Color) color.Color {
	if row.free || i >= len(colors) {
		return styles.TextMutedGray
	}
	return colors[i]
}

// contextHeaderMeta returns the "model  •  limit" line under the title. In
// the edge case where only the compaction model's window is resolvable (the
// primary model's own window is unknown), the limit re-attributes to it so
// the figure isn't silently presented as the primary model's.
func contextHeaderMeta(b *runtime.ContextBreakdown) string {
	var parts []string
	if b.Model != "" {
		parts = append(parts, b.Model)
	}
	switch {
	case b.ContextLimit <= 0:
		parts = append(parts, "context limit unknown")
	case b.PrimaryContextLimit == 0 && b.CompactionContextLimit > 0 && b.ContextLimit == b.CompactionContextLimit:
		parts = append(parts, "limit: "+formatTokenCount(b.ContextLimit)+" tokens (from compaction model)")
	default:
		parts = append(parts, "limit: "+formatTokenCount(b.ContextLimit)+" tokens")
	}
	return strings.Join(parts, "  •  ")
}

// contextCompactionMeta returns the second header line attributing a capped
// effective limit to the dedicated compaction model that imposes it, or ""
// when the limit isn't actually capped (no dedicated model, or its window is
// unresolvable or not smaller than the primary model's). Deliberately terse
// (no primary-window parenthetical): naming which model's window is being
// compared confused readers more than it clarified (see plan refinement).
func contextCompactionMeta(b *runtime.ContextBreakdown) string {
	if b.CompactionContextLimit <= 0 || b.CompactionContextLimit >= b.PrimaryContextLimit {
		return ""
	}
	return fmt.Sprintf("compaction cap: %s • %s tokens",
		b.CompactionModel,
		formatTokenCount(b.CompactionContextLimit))
}

// usageSummary is the line under the bar: "~24.5K of 128.0K tokens (19%)",
// or just the estimated total when the limit is unknown.
func usageSummary(b *runtime.ContextBreakdown) string {
	total := b.TotalTokens()
	if b.ContextLimit <= 0 {
		return "~" + formatTokenCount(total) + " tokens estimated"
	}
	return fmt.Sprintf("~%s of %s tokens (%s)",
		formatTokenCount(total),
		formatTokenCount(b.ContextLimit),
		percentLabel(total, scaleTokens(b)))
}

// renderContextBar renders the stacked usage bar: one colored segment per
// category, proportional to its share of scale, with the remainder drawn
// as muted free-space cells. Cumulative rounding keeps the bar exactly
// barWidth cells wide with no drift.
func renderContextBar(rows []contextRow, scale int64, barWidth int) string {
	if barWidth < 1 || scale <= 0 {
		return ""
	}
	colors := categoryColors()
	var bar strings.Builder
	cells := 0
	var cum int64
	for i, row := range rows {
		if row.free {
			continue
		}
		cum += row.tokens
		end := int(float64(cum) / float64(scale) * float64(barWidth))
		if n := min(end, barWidth) - cells; n > 0 {
			bar.WriteString(lipgloss.NewStyle().
				Foreground(markerColor(i, row, colors)).
				Render(strings.Repeat(contextBarFilled, n)))
			cells += n
		}
	}
	if n := barWidth - cells; n > 0 {
		bar.WriteString(styles.MutedStyle.Render(strings.Repeat(contextBarFree, n)))
	}
	return bar.String()
}

// contextLabelWidth returns the widest row label, so token columns align.
func contextLabelWidth(rows []contextRow) int {
	width := 0
	for _, row := range rows {
		width = max(width, len(row.label))
	}
	return width
}

// renderContextRow renders one category line:
// "■ Tool definitions   8.4K   7%  (23 tools)".
func renderContextRow(row *contextRow, scale int64, labelWidth int, markerCol color.Color) string {
	marker := lipgloss.NewStyle().Foreground(markerCol).Render(contextRowMarker)
	label := row.label + strings.Repeat(" ", labelWidth-len(row.label))
	if row.free {
		label = styles.MutedStyle.Render(label)
	} else {
		label = labelStyle().Render(label)
	}
	line := fmt.Sprintf("%s %s  %s  %s",
		marker,
		label,
		valueStyle().Render(padRight(formatTokenCount(row.tokens))),
		valueStyle().Render(fmt.Sprintf("%4s", percentLabel(row.tokens, scale))))
	if suffix := row.itemsSuffix(); suffix != "" {
		line += "  " + styles.MutedStyle.Render(suffix)
	}
	return line
}

func (d *contextDialog) applyScrolling(allLines []string, contentWidth, _ int) string {
	width, _, _ := d.dialogSize()
	footer := d.RenderPickerFooter(contentWidth+d.scrollview.ReservedCols(), actionsForKeys(d.helpKeys()...)...)
	return d.RenderScrollableBody(styles.DialogStyle, width, strings.Join(allLines[:contextHeaderLines], "\n"), strings.Join(allLines[contextHeaderLines:], "\n"), footer)
}

// helpKeys returns the footer bindings; the selection pair appears while
// selectable rows are listed, the compact key while live sessions are
// listed, and the drop key while attached files are listed.
func (d *contextDialog) helpKeys() []string {
	keys := []string{"↑↓", "scroll"}
	if d.selectableCount() > 0 {
		keys = []string{"↑↓", "select"}
	}
	if _, ok := d.selectedLiveSession(); ok {
		keys = append(keys, "Enter", "compact selected")
	}
	if _, ok := d.selectedAttachedIndex(); ok {
		keys = append(keys, "d", "drop attachment")
	}
	return append(keys, "c", "copy")
}

// ---------------------------------------------------------------------------
// Plain-text rendering (clipboard copy)
// ---------------------------------------------------------------------------

func (d *contextDialog) renderPlainText() string {
	b := d.breakdown
	rows := contextRows(b)
	scale := scaleTokens(b)
	labelWidth := contextLabelWidth(rows)

	lines := []string{"Context Window"}
	if meta := contextHeaderMeta(b); meta != "" {
		lines = append(lines, meta)
	}
	if compactionMeta := contextCompactionMeta(b); compactionMeta != "" {
		lines = append(lines, compactionMeta)
	}
	lines = append(lines, "", usageSummary(b), "")

	for _, row := range rows {
		line := fmt.Sprintf("%s  %-8s %4s",
			row.label+strings.Repeat(" ", labelWidth-len(row.label)),
			formatTokenCount(row.tokens),
			percentLabel(row.tokens, scale))
		if suffix := row.itemsSuffix(); suffix != "" {
			line += "  " + suffix
		}
		lines = append(lines, line)
	}

	if len(d.liveSessions) > 0 {
		lines = append(lines, "", "Live sessions")
		for i := range d.liveSessions {
			lines = append(lines, plainLiveSessionLine(&d.liveSessions[i]))
		}
	}

	if len(b.AttachedFiles) > 0 {
		lines = append(lines, "", "Attached files")
		for i := range b.AttachedFiles {
			lines = append(lines, plainFileLine(&b.AttachedFiles[i], scale))
		}
	}
	if len(b.PromptFileItems) > 0 {
		lines = append(lines, "", "Prompt files")
		for i := range b.PromptFileItems {
			lines = append(lines, plainFileLine(&b.PromptFileItems[i], scale))
		}
	}

	if b.LatestCompactionSummary != "" {
		lines = append(lines, "", contextSummarySection, b.LatestCompactionSummary)
	}

	lines = append(lines, "", contextEstimateNote)
	return strings.Join(lines, "\n")
}

// plainFileLine formats one inventory file for the clipboard copy.
func plainFileLine(file *runtime.ContextFile, scale int64) string {
	line := fmt.Sprintf("%-8s %4s  %s", fileTokensLabel(file), percentLabel(file.Tokens, scale), file.Path)
	if file.Missing {
		line += " (missing)"
	}
	return line
}

// plainLiveSessionLine formats one live-session row for the clipboard copy.
func plainLiveSessionLine(row *runtime.LiveSession) string {
	line := fmt.Sprintf("%s  %s  %s", row.AgentName, row.ShortID(), liveSessionBudget(row))
	if row.Current {
		line += " (current)"
	}
	return line
}

func (d *contextDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.renderBody(true)
	return cmd
}
