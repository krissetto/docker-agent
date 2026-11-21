// Package workblock renders a compact, expandable group of source-owned TUI views.
package workblock

import (
	"reflect"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	foldPreviewLines = 3
	// workPreviewLines is how many of the most recent reasoning lines a collapsed
	// Work block shows, matching the compact thinking preview.
	workPreviewLines = 3
)

// nestedReasoning is a reasoning child. Its tail previews the collapsed Work
// block. While hosted here it renders its full text with no header or disclosure.
type nestedReasoning interface {
	PreviewLines() (lines []string, truncated bool)
	ReasoningActive() bool
	AdoptWorkOwner(owner any)
	ReleaseWorkOwner(owner any)
}

// SourceContentWidth reserves an inset on both sides of expanded Work rows.
func SourceContentWidth(width int) int {
	return max(0, width-2*BodyMargin(width))
}

func BodyMargin(width int) int {
	switch {
	case width >= 10:
		return 2
	case width >= 4:
		return 1
	default:
		return 0
	}
}

// Item references a source-owned child view; its width follows this composite.
type Item struct {
	SourceIndex int
	View        layout.Model
}

// Status describes compact header data without inferring completion semantics.
type Status struct {
	Active            bool
	ToolCount         int
	BackgroundRunning int
	BackgroundFailed  bool
	// Elapsed is the authoritative turn duration (settled turns only); zero means unknown.
	Elapsed time.Duration
	// Outcome is the settled turn's outcome: "" (completed), "stopped" or "failed".
	Outcome string
	// Waiting replaces the active label while a human interaction blocks the turn,
	// e.g. "Waiting for approval". The spinner pauses while it is set.
	Waiting string
}

// Turn outcomes shown in the settled header.
const (
	OutcomeStopped = "stopped"
	OutcomeFailed  = "failed"
)

// minSettledLabel hides sub-second durations: they are noise in a header.
const minSettledLabel = time.Second

// Row is a block-local line range occupied by one source child; End is exclusive.
type Row struct {
	SourceIndex int
	Start       int
	End         int
}

type cachedItem struct {
	item         Item
	sourceWidth  int
	text         string
	rendered     bool
	bodyStart    int
	bodyHeight   int
	contentStart int
}

// Model is a thin display composite; source views remain owned and updated by the caller.
type Model struct {
	ar               *animation.Runtime
	spinner          spinner.Spinner
	disclosure       agentmessage.Disclosure
	fold             animation.Transition
	previewFade      animation.Transition
	previewRetire    animation.Transition
	previewActive    bool
	previewRetired   bool
	previewLines     []string
	previewFading    []bool
	previewTruncated bool
	previewSuspended bool
	previewRows      int
	items            []cachedItem
	status           Status
	width            int
	height           int
	expanded         bool
	foldLines        []string
	foldRows         int
	foldFrom         int
	foldSource       int

	bodyValid        bool
	body             string
	bodyRows         int
	rows             []Row
	viewValid        bool
	view             string
	visualGeneration uint64
}

// New creates an initially collapsed Work block.
func New(ar *animation.Runtime) *Model {
	if ar == nil {
		panic("workblock: nil animation runtime")
	}
	return &Model{
		ar: ar, spinner: spinner.New(ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle),
		disclosure: agentmessage.New(ar), fold: ar.Transition(), previewFade: ar.Transition(), previewRetire: ar.Transition(), previewRetired: true, width: 80,
	}
}

// SetItems refreshes source references. Replaced pointers invalidate cached output.
func (m *Model) SetItems(items []Item) {
	previous := make(map[int]cachedItem, len(m.items))
	for _, item := range m.items {
		previous[item.item.SourceIndex] = item
	}
	next := make([]cachedItem, len(items))
	for i, item := range items {
		if cached, ok := previous[item.SourceIndex]; ok && sameView(item.View, cached.item.View) {
			cached.item = item
			next[i] = cached
		} else {
			next[i].item = item
			next[i].sourceWidth = -1
		}
		if child, ok := item.View.(nestedReasoning); ok {
			child.AdoptWorkOwner(m)
		}
		m.sizeSource(&next[i])
	}
	// A child that left this block stops being hosted here. One that moved into
	// another block was adopted there and keeps that host.
	current := make(map[int]bool, len(items))
	for _, item := range items {
		current[item.SourceIndex] = true
	}
	for _, old := range m.items {
		if child, ok := old.item.View.(nestedReasoning); ok && !current[old.item.SourceIndex] {
			child.ReleaseWorkOwner(m)
		}
	}
	m.items = next
	m.invalidateBody()
}

func sameView(a, b layout.Model) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() {
		return false
	}
	if av.Type().Comparable() {
		return av.Interface() == bv.Interface()
	}
	if av.Kind() == reflect.Pointer {
		return av.Pointer() == bv.Pointer()
	}
	return false
}

func (m *Model) sizeSource(item *cachedItem) {
	width := SourceContentWidth(m.width)
	// The transcript resizes every view, Work children included, outside this
	// composite; re-apply the body width so a re-render never uses that width.
	if child, ok := item.item.View.(interface {
		SetSize(width, height int) tea.Cmd
	}); ok {
		child.SetSize(width, 0)
	}
	if item.sourceWidth == width {
		return
	}
	item.sourceWidth = width
	item.rendered = false
	item.text = ""
	item.bodyStart, item.bodyHeight = 0, 0
}

// Invalidate drops cached child output and row geometry after a source mutation.
func (m *Model) Invalidate() {
	for i := range m.items {
		m.items[i].rendered = false
		m.items[i].text = ""
		m.items[i].bodyStart, m.items[i].bodyHeight = 0, 0
		m.sizeSource(&m.items[i])
	}
	m.invalidateBody()
}

// InvalidateSource marks one child as changed without discarding other cached child renders.
func (m *Model) InvalidateSource(sourceIndex int) {
	for i := range m.items {
		if m.items[i].item.SourceIndex == sourceIndex {
			m.items[i].rendered = false
			m.items[i].text = ""
			m.items[i].bodyStart, m.items[i].bodyHeight = 0, 0
			break
		}
	}
	m.invalidateBody()
}

func (m *Model) invalidateBody() {
	m.refreshPreview()
	m.bodyValid = false
	m.body = ""
	m.bodyRows = 0
	m.rows = nil
	m.invalidateView()
}

func (m *Model) invalidateView() {
	m.viewValid = false
	m.view = ""
	m.visualGeneration++
}

// VisualGeneration changes only when this composite's rendered output changes.
func (m *Model) VisualGeneration() uint64 { return m.visualGeneration }

func (m *Model) clearBody() {
	m.Invalidate()
}

// SetStatus updates compact header data and registers/unregisters its spinner as needed.
func (m *Model) SetStatus(status Status) tea.Cmd {
	if m.status == status {
		return nil
	}
	wasSpinning := m.spinning()
	m.status = status
	m.refreshPreview()
	m.invalidateView()
	if spinning := m.spinning(); spinning && !wasSpinning && !m.previewSuspended {
		return m.spinner.Init()
	} else if !spinning && wasSpinning {
		m.spinner.Stop()
	}
	return nil
}

// spinning reports whether the header spinner should run: an active turn that
// is not blocked on a human interaction.
func (m *Model) spinning() bool { return m.status.Active && m.status.Waiting == "" }

func (m *Model) Status() Status { return m.status }

func (m *Model) Expanded() bool   { return m.expanded }
func (m *Model) IsExpanded() bool { return m.expanded }

// SetExpandedImmediately settles an explicit navigation reveal without an
// intermediate height, so callers can safely compute child hit geometry.
func (m *Model) SetExpandedImmediately(expanded bool) {
	if m.expanded == expanded && !m.disclosure.NeedsTick() {
		return
	}
	if m.expanded != expanded {
		m.disclosure.Toggle()
	}
	m.expanded = expanded
	m.snapPreview()
	m.fold.Cancel()
	m.foldLines = nil
	m.foldRows = 0
	m.disclosure.Settle()
	m.Invalidate()
}

// SetExpanded uses the shared disclosure's canonical 140ms reveal.
func (m *Model) SetExpanded(expanded bool) {
	if m.expanded == expanded {
		return
	}
	m.expanded = expanded
	m.snapPreview()
	m.fold.Cancel()
	m.foldLines = nil
	m.foldRows = 0
	m.disclosure.Toggle()
	m.invalidateView()
}

func (m *Model) Toggle() { m.SetExpanded(!m.expanded) }

// FoldCandidate shows a short-lived, inert excerpt from a departing response.
// It deliberately has no source hit mapping: excerpts are not the full source view.
func (m *Model) FoldCandidate(sourceIndex int, excerpt string) tea.Cmd {
	return m.FoldCandidateLines(sourceIndex, boundedExcerpt(excerpt, max(1, m.width-4)))
}

// FoldCandidateLines temporarily preserves the visible portion of a departing
// answer while its full source row moves into Work. Lines are inert: hit testing
// continues to resolve only actual source child rows.
func (m *Model) FoldCandidateLines(sourceIndex int, lines []string) tea.Cmd {
	if m.expanded || len(lines) == 0 {
		return nil
	}
	m.foldSource = sourceIndex
	m.foldLines = make([]string, len(lines))
	for i, line := range lines {
		m.foldLines[i] = styles.MutedStyle.Render(ansi.Truncate(ansi.Strip(line), min(max(1, m.width-4), 120), "…"))
	}
	m.foldFrom = len(m.foldLines)
	m.foldRows = m.foldFrom
	cmd := m.fold.Start(animation.ShortDuration, animation.EaseOutCubic)
	m.invalidateView()
	return cmd
}

func boundedExcerpt(excerpt string, width int) []string {
	// Cap before stripping/truncating so one huge streamed line stays cheap to summarize.
	const maxExcerptBytes = 4096
	if len(excerpt) > maxExcerptBytes {
		excerpt = excerpt[:maxExcerptBytes]
		excerpt = strings.ToValidUTF8(excerpt, "�")
	}
	lines := strings.SplitN(strings.TrimSpace(excerpt), "\n", foldPreviewLines+1)
	if len(lines) > foldPreviewLines {
		lines = lines[:foldPreviewLines]
	}
	for i, line := range lines {
		line = ansi.Strip(line)
		lines[i] = styles.MutedStyle.Render(ansi.Truncate(line, min(width, 120), "…"))
	}
	return lines
}

func (m *Model) Init() tea.Cmd {
	if m.spinning() && !m.previewSuspended {
		return m.spinner.Init()
	}
	return nil
}

// NeedsTick reports animation work owned by the composite.
func (m *Model) NeedsTick() bool {
	return (!m.previewSuspended && m.spinning()) || m.disclosure.NeedsTick() || m.fold.Running() || m.previewFade.Running() || m.previewRetire.Running()
}

// ResumeAnimation reacquires the composite's status spinner after a pause.
func (m *Model) ResumeAnimation() tea.Cmd {
	m.previewSuspended = false
	m.snapPreview()
	if m.spinning() {
		return m.spinner.Init()
	}
	return nil
}

func (m *Model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if tick, ok := msg.(animation.TickMsg); ok {
		if m.previewRetire.Running() {
			m.previewRetire.Tick()
			if !m.previewRetire.Running() {
				m.previewRetired = true
				m.previewRows = 0
				m.previewLines = nil
				m.previewFading = nil
			}
			tick.MarkDirty()
			m.invalidateView()
		}
		if m.previewFade.Running() {
			m.previewFade.Tick()
			tick.MarkDirty()
			m.invalidateView()
		}
		before := m.foldRows
		if m.fold.Running() {
			m.fold.Tick()
			m.foldRows = int(float64(m.foldFrom)*(1-m.fold.Value()) + 0.5)
			if m.foldRows != before {
				tick.MarkDirty()
				m.invalidateView()
			}
			if !m.fold.Running() {
				m.foldRows = 0
				m.foldLines = nil
				m.invalidateView()
			}
		}
		if m.disclosure.NeedsTick() {
			m.disclosure.Tick(tick)
			m.invalidateView()
			if !m.disclosure.NeedsTick() && !m.expanded {
				m.clearBody()
			}
		}
	}
	if m.spinning() && !m.previewSuspended {
		_, cmd := m.spinner.Update(msg)
		if _, ok := msg.(animation.TickMsg); ok {
			m.invalidateView()
		}
		return m, cmd
	}
	return m, nil
}

func (m *Model) SetSize(width, height int) tea.Cmd {
	if width != m.width {
		m.width = width
		for i := range m.items {
			m.items[i].sourceWidth = -1
		}
		m.disclosure.Settle()
		m.fold.Cancel()
		m.foldRows = 0
		m.foldLines = nil
		m.Invalidate()
		m.snapPreview()
	}
	m.height = height
	return nil
}

func (m *Model) GetSize() (int, int) { return m.width, m.height }

func (m *Model) ensureBody() {
	if m.bodyValid || (!m.expanded && !m.disclosure.Visible()) {
		return
	}
	var parts []string
	line := 0
	for i := range m.items {
		m.sizeSource(&m.items[i])
		if !m.items[i].rendered {
			if child := m.items[i].item.View; child != nil {
				m.items[i].text = child.View()
			}
			m.items[i].rendered = true
		}
		text := m.items[i].text
		if text == "" {
			m.items[i].bodyHeight = 0
			continue
		}
		if line == 0 {
			// Breathing room between the Work header and its first child.
			parts = append(parts, "")
			line++
		}
		if line > 1 && !blankLine(parts[len(parts)-1]) && !firstLineBlank(text) {
			parts = append(parts, "")
			line++
		}
		m.items[i].bodyStart = line
		m.items[i].contentStart = line
		m.items[i].bodyHeight = lipgloss.Height(text)
		parts = append(parts, m.insetBody(text)...)
		line += m.items[i].bodyHeight
	}
	m.body = strings.Join(parts, "\n")
	m.bodyRows = len(strings.Split(m.body, "\n"))
	m.bodyValid = true
}

func blankLine(line string) bool { return strings.TrimSpace(ansi.Strip(line)) == "" }

func firstLineBlank(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	return blankLine(line)
}

func (m *Model) insetBody(text string) []string {
	left, right := m.bodyMargins()
	innerWidth := max(0, m.width-left-right)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		padding := max(0, innerWidth-ansi.StringWidth(line))
		lines[i] = strings.Repeat(" ", left) + line + strings.Repeat(" ", padding+right)
	}
	return lines
}

func (m *Model) bodyMargins() (left, right int) {
	margin := BodyMargin(m.width)
	return margin, margin
}

func (m *Model) View() string {
	if m.viewValid {
		return m.view
	}
	parts := []string{m.header()}
	m.rows = nil
	m.ensureBody()
	if m.disclosure.Visible() && m.body != "" {
		revealed := m.disclosure.Reveal(m.body)
		visibleRows := lipgloss.Height(revealed)
		if revealed != "" {
			parts = append(parts, revealed)
		}
		for i := range m.items {
			item := m.items[i]
			if item.text == "" || item.bodyHeight <= 0 {
				continue
			}
			start, end := item.bodyStart, item.contentStart+item.bodyHeight
			if start >= visibleRows {
				continue
			}
			end = min(end, visibleRows)
			if end > start {
				contentStart := max(start, item.contentStart)
				if end > contentStart {
					m.rows = append(m.rows, Row{SourceIndex: item.item.SourceIndex, Start: 1 + contentStart, End: 1 + end})
				}
			}
		}
	}
	if !m.expanded && !m.disclosure.Visible() {
		if preview := m.collapsedPreview(); preview != "" {
			parts = append(parts, preview)
		}
	}
	if m.foldRows > 0 && len(m.foldLines) > 0 {
		parts = append(parts, m.foldLines[:min(m.foldRows, len(m.foldLines))]...)
	}
	m.view = strings.Join(parts, "\n")
	m.viewValid = true
	return m.view
}

// refreshPreview runs on authoritative source mutations, never on View or ticks.
// Running reveal/retirement transitions keep their deadlines across projection
// rebuilds. Displayable history keeps its compact tail; only live reasoning
// reserves the full preview row budget.
func (m *Model) refreshPreview() {
	var lines []string
	truncated := false
	active := false
	for i := range m.items {
		if child, ok := m.items[i].item.View.(nestedReasoning); ok {
			childLines, childTruncated := child.PreviewLines()
			active = active || (child.ReasoningActive() && len(childLines) > 0)
			lines = append(lines, childLines...)
			truncated = truncated || childTruncated
		}
	}
	if len(lines) > workPreviewLines {
		truncated = true
		lines = lines[len(lines)-workPreviewLines:]
	}
	fading := make([]bool, len(lines))
	// Match the old tail against the new head. A growing last line is the
	// same row, not a fresh arrival; shifted rows retain their opacity.
	matched := 0
	for n := min(len(m.previewLines), len(lines)); n > 0; n-- {
		start := len(m.previewLines) - n
		ok := true
		for i := range n {
			old := m.previewLines[start+i]
			if lines[i] != old && !(start+i == len(m.previewLines)-1 && strings.HasPrefix(lines[i], old)) {
				ok = false
				break
			}
		}
		if ok {
			matched = n
			for i := range n {
				fading[i] = m.previewFade.Running() && m.previewFading[start+i]
			}
			break
		}
	}
	wasActive := m.previewActive
	m.previewActive = active && m.status.Active && len(lines) > 0
	if len(lines) > 0 {
		m.previewRetire.Cancel()
		m.previewRetired = false
		m.previewRows = len(lines)
		if m.previewActive {
			m.previewRows = workPreviewLines
		}
	} else if (wasActive || len(m.previewLines) > 0) && !m.previewRetired {
		m.previewFade.Cancel()
		if !m.expanded && !m.disclosure.Visible() && !m.previewSuspended && m.previewRows > 0 {
			if !m.previewRetire.Running() {
				m.previewRetire.Start(animation.ShortDuration, animation.EaseOutCubic)
			}
		} else {
			m.previewRetired = true
			m.previewRows = 0
		}
	}
	live := m.previewActive && !m.expanded && !m.disclosure.Visible() && !m.previewSuspended
	if live && matched < len(lines) {
		for i := matched; i < len(lines); i++ {
			fading[i] = true
		}
		if !m.previewFade.Running() {
			m.previewFade.Start(animation.ShortDuration, animation.EaseOutCubic)
		}
	} else if !live || len(lines) == 0 {
		m.previewFade.Cancel()
	}
	// Source resets and removals must not replace retiring paint.
	// Keep only the bounded old tail through its existing retirement deadline.
	if !m.previewActive && m.previewRetire.Running() {
		return
	}
	m.previewLines, m.previewFading, m.previewTruncated = lines, fading, truncated
}

func (m *Model) snapPreview() {
	m.previewFade.Cancel()
	if m.previewRetire.Running() {
		m.previewRetire.Cancel()
		m.previewRetired = true
		m.previewRows = 0
	}
	clear(m.previewFading)
	m.invalidateView()
}

func (m *Model) collapsedPreview() string {
	if m.previewRetired || (len(m.previewLines) == 0 && m.previewRows == 0) {
		return ""
	}
	lines := make([]string, len(m.previewLines))
	for i, line := range m.previewLines {
		style := styles.MutedStyle.Italic(true)
		if i == 0 && m.previewTruncated {
			style = style.Faint(true)
		}
		lines[i] = style.Render(ansi.Truncate(line, SourceContentWidth(m.width), ""))
		if m.previewRetire.Running() {
			lines[i] = styles.FadeLine(lines[i], 1-m.previewRetire.Value())
		} else if m.previewFade.Running() && m.previewFading[i] {
			lines[i] = styles.FadeLine(lines[i], m.previewFade.Value())
		}
	}
	// Reserve the bounded live budget once, instead of repeatedly growing and
	// shrinking the transcript (and its bottom-slack animation) between chunks.
	if m.previewActive || m.previewRetire.Running() {
		for len(lines) < m.previewRows {
			lines = append(lines, "")
		}
	}
	return strings.Join(m.insetBody(strings.Join(lines, "\n")), "\n")
}

func (m *Model) Height() int { return lipgloss.Height(m.View()) }

func (m *Model) Rows() []Row {
	m.View()
	return append([]Row(nil), m.rows...)
}

func (m *Model) Items() []Item {
	items := make([]Item, len(m.items))
	for i, item := range m.items {
		items[i] = item.item
	}
	return items
}

// SourceAt maps only visible, expanded child rows; fold excerpts are inert.
func (m *Model) SourceAt(line int) (sourceIndex, localLine int, ok bool) {
	m.View()
	for _, row := range m.rows {
		if line >= row.Start && line < row.End {
			return row.SourceIndex, line - row.Start, true
		}
	}
	return 0, 0, false
}

// SourceColumnAt maps a composite display column to the source child's column.
func (m *Model) SourceColumnAt(line, col int) (int, bool) {
	left, right := m.bodyMargins()
	if _, _, ok := m.SourceAt(line); !ok || col < left || col >= m.width-right {
		return 0, false
	}
	return col - left, true
}

func (m *Model) WheelAt(line, col, delta int) bool {
	source, local, ok := m.SourceAt(line)
	if !ok {
		return false
	}
	child, ok := m.sourceView(source).(interface {
		WheelAt(line, col, delta int) bool
	})
	childCol, inside := m.SourceColumnAt(line, col)
	return ok && inside && child.WheelAt(local, childCol, delta)
}

func (m *Model) SetHoveredAt(line int, hovered bool) bool {
	return m.setHover(line, -1, hovered, false)
}

// SetHoveredPositionAt routes gutter-precise hover to the owning child. Columns
// outside the child's inset clear its pointer instead of hovering it.
func (m *Model) SetHoveredPositionAt(line, col int, hovered bool) bool {
	return m.setHover(line, col, hovered, true)
}

func (m *Model) setHover(line, col int, hovered, positional bool) bool {
	source, local, found := 0, 0, false
	if hovered {
		source, local, found = m.SourceAt(line)
		if positional {
			var inside bool
			col, inside = m.SourceColumnAt(line, col)
			if !inside {
				col = -1
			}
		}
	}
	changed := false
	for i := range m.items {
		item := m.items[i].item
		child, ok := item.View.(interface {
			SetHoveredAt(line int, hovered bool) bool
		})
		if !ok {
			continue
		}
		childChanged := false
		if found && item.SourceIndex == source {
			if pointer, ok := child.(interface {
				SetHoveredPositionAt(line, col int, hovered bool) bool
			}); positional && ok {
				childChanged = pointer.SetHoveredPositionAt(local, col, true)
			} else {
				childChanged = child.SetHoveredAt(local, true)
			}
		} else {
			childChanged = child.SetHoveredAt(-1, false)
		}
		if childChanged {
			// Hover changes the child's rendered text, not just the outer slot.
			m.InvalidateSource(item.SourceIndex)
			changed = true
		}
	}
	return changed
}

func (m *Model) sourceView(source int) layout.Model {
	for i := range m.items {
		if m.items[i].item.SourceIndex == source {
			return m.items[i].item.View
		}
	}
	return nil
}

func (m *Model) IsToggleLine(line int) bool { return line == 0 }
func (m *Model) IsToggleAt(line, col int) bool {
	if line == 0 {
		return col >= 0 && col < m.width
	}
	source, local, ok := m.SourceAt(line)
	if !ok {
		return false
	}
	child, ok := m.sourceView(source).(interface{ IsToggleAt(line, col int) bool })
	childCol, inside := m.SourceColumnAt(line, col)
	return ok && inside && child.IsToggleAt(local, childCol)
}

func (m *Model) ToggleAt(line, col int) {
	if line == 0 && col >= 0 && col < m.width {
		m.Toggle()
		return
	}
	source, local, ok := m.SourceAt(line)
	if !ok {
		return
	}
	child := m.sourceView(source)
	childCol, inside := m.SourceColumnAt(line, col)
	if !inside {
		return
	}
	if toggleAt, ok := child.(interface{ ToggleAt(line, col int) }); ok {
		toggleAt.ToggleAt(local, childCol)
		return
	}
	if toggle, ok := child.(interface {
		IsToggleAt(line, col int) bool
		Toggle()
	}); ok && toggle.IsToggleAt(local, childCol) {
		toggle.Toggle()
	}
}

// Release detaches the nested reasoning children this block still references,
// so they render standalone again once the block is dropped.
func (m *Model) Release() {
	for _, item := range m.items {
		if child, ok := item.item.View.(nestedReasoning); ok {
			child.ReleaseWorkOwner(m)
		}
	}
}

func (m *Model) StopAnimation() {
	m.previewSuspended = true
	m.snapPreview()
	m.spinner.Stop()
	m.disclosure.Settle()
	m.fold.Cancel()
	m.foldRows = 0
	m.foldLines = nil
	m.invalidateView()
}

// settledLabel is the settled-turn label. A duration is shown only when it is
// known and at least a second. A stopped or failed outcome stays explicit even
// without a usable duration; a completed turn with no usable duration is unlabelled.
func (m *Model) settledLabel() string {
	if m.status.Active {
		return ""
	}
	if m.status.Elapsed < minSettledLabel {
		switch m.status.Outcome {
		case OutcomeStopped:
			return "Stopped"
		case OutcomeFailed:
			return "Failed"
		default:
			return ""
		}
	}
	duration := m.status.Elapsed.Round(time.Second).String()
	switch m.status.Outcome {
	case OutcomeStopped:
		return "Stopped after " + duration
	case OutcomeFailed:
		return "Failed after " + duration
	default:
		return duration
	}
}

// HeaderView renders only the disclosure/status row, without materializing its body.
func (m *Model) HeaderView() string { return m.header() }

func (m *Model) header() string {
	label := "Work"
	if m.status.Active {
		label = "Working"
	}
	if m.status.Waiting != "" {
		label = m.status.Waiting
	}
	indicator := "[+]"
	if m.expanded {
		indicator = "[-]"
	}
	prefix := "  "
	if m.spinning() {
		prefix += m.spinner.View() + " "
	}
	parts := []string{prefix + styles.WorkHeaderStyle.Render(label) + styles.MutedStyle.Bold(true).Render(" "+indicator)}
	if m.status.ToolCount > 0 {
		unit := "tools"
		if m.status.ToolCount == 1 {
			unit = "tool"
		}
		parts = append(parts, styles.MutedStyle.Render("("+strconv.Itoa(m.status.ToolCount)+" "+unit+")"))
	}
	if m.status.BackgroundRunning > 0 {
		parts = append(parts, styles.MutedStyle.Render(strconv.Itoa(m.status.BackgroundRunning)+" running in bg"))
	}
	if m.status.BackgroundFailed {
		parts = append(parts, styles.ToolErrorMessageStyle.Render("bg failed"))
	}
	if text := m.settledLabel(); text != "" {
		parts = append(parts, styles.MutedStyle.Render("· "+text))
	}
	return ansi.Truncate(strings.Join(parts, " "), max(0, m.width), "")
}
