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

const foldPreviewLines = 3

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
	Active    bool
	ToolCount int
	Elapsed   time.Duration
}

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
	ar         *animation.Runtime
	spinner    spinner.Spinner
	disclosure agentmessage.Disclosure
	fold       animation.Transition
	items      []cachedItem
	status     Status
	width      int
	height     int
	expanded   bool
	foldLines  []string
	foldRows   int
	foldFrom   int
	foldSource int

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
		disclosure: agentmessage.New(ar), fold: ar.Transition(), width: 80,
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
		m.sizeSource(&next[i])
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
	wasActive := m.status.Active
	m.status = status
	m.invalidateView()
	if status.Active && !wasActive {
		return m.spinner.Init()
	}
	if !status.Active && wasActive {
		m.spinner.Stop()
	}
	return nil
}

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
	if m.status.Active {
		return m.spinner.Init()
	}
	return nil
}

// NeedsTick reports animation work owned by the composite.
func (m *Model) NeedsTick() bool {
	return m.status.Active || m.disclosure.NeedsTick() || m.fold.Running()
}

// ResumeAnimation reacquires the composite's status spinner after a pause.
func (m *Model) ResumeAnimation() tea.Cmd {
	if m.status.Active {
		return m.spinner.Init()
	}
	return nil
}

func (m *Model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if tick, ok := msg.(animation.TickMsg); ok {
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
	if m.status.Active {
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
	if m.foldRows > 0 && len(m.foldLines) > 0 {
		parts = append(parts, m.foldLines[:min(m.foldRows, len(m.foldLines))]...)
	}
	m.view = strings.Join(parts, "\n")
	m.viewValid = true
	return m.view
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
	if !hovered {
		changed := false
		for i := range m.items {
			if child, ok := m.items[i].item.View.(interface {
				SetHoveredAt(line int, hovered bool) bool
			}); ok {
				changed = child.SetHoveredAt(-1, false) || changed
			}
		}
		return changed
	}
	source, local, ok := m.SourceAt(line)
	if !ok {
		return false
	}
	child, ok := m.sourceView(source).(interface {
		SetHoveredAt(line int, hovered bool) bool
	})
	return ok && child.SetHoveredAt(local, true)
}

// SetHoveredPositionAt routes gutter-precise hover to the owning child. Columns
// outside the child's inset clear its pointer instead of hovering it.
func (m *Model) SetHoveredPositionAt(line, col int, hovered bool) bool {
	if !hovered {
		return m.SetHoveredAt(line, false)
	}
	source, local, ok := m.SourceAt(line)
	if !ok {
		return m.SetHoveredAt(-1, false)
	}
	childCol, inside := m.SourceColumnAt(line, col)
	if !inside {
		childCol = -1
	}
	changed := false
	for i := range m.items {
		if m.items[i].item.SourceIndex == source {
			continue
		}
		if child, ok := m.items[i].item.View.(interface {
			SetHoveredAt(line int, hovered bool) bool
		}); ok {
			changed = child.SetHoveredAt(-1, false) || changed
		}
	}
	child, ok := m.sourceView(source).(interface {
		SetHoveredAt(line int, hovered bool) bool
	})
	if !ok {
		return changed
	}
	if positional, ok := child.(interface {
		SetHoveredPositionAt(line, col int, hovered bool) bool
	}); ok {
		return positional.SetHoveredPositionAt(local, childCol, true) || changed
	}
	return child.SetHoveredAt(local, true) || changed
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

func (m *Model) StopAnimation() {
	m.spinner.Stop()
	m.disclosure.Settle()
	m.fold.Cancel()
	m.foldRows = 0
	m.foldLines = nil
	m.invalidateView()
}

func (m *Model) header() string {
	label := "Work"
	if m.status.Active {
		label = "Working"
	}
	indicator := "[+]"
	if m.expanded {
		indicator = "[-]"
	}
	prefix := "  "
	if m.status.Active {
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
	if !m.status.Active && m.status.Elapsed > 0 {
		parts = append(parts, styles.MutedStyle.Render("· "+m.status.Elapsed.Round(time.Second).String()))
	}
	return ansi.Truncate(strings.Join(parts, " "), max(0, m.width), "")
}
