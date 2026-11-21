package messages

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// selectionState encapsulates all state related to text selection
type selectionState struct {
	active          bool
	startLine       int
	startCol        int
	endLine         int
	endCol          int
	mouseButtonDown bool
	dragged         bool // the pointer moved away from the press since the last click
	mouseY          int  // Screen Y coordinate for autoscroll

	// Word anchor for double-click drags: while anchored, the selection
	// always covers at least the anchored word and extends around it.
	anchored        bool
	anchorStartLine int
	anchorStartCol  int
	anchorEndLine   int
	anchorEndCol    int

	// Multi-click detection
	lastClickTime time.Time
	lastClickLine int
	lastClickCol  int
	clickCount    int // 1=single, 2=double, 3=triple

	// Debounced copy: incremented on each click, copy only fires if ID matches
	pendingCopyID int

	// refs holds the content position of each endpoint while a width or theme
	// epoch has not been placed yet: start, end, anchor start, anchor end.
	// Global rows above stay authoritative whenever a ref is nil.
	refs [4]*selectionRef
}

// selectionRef is the content position of one selection endpoint, captured
// at an epoch. line and col are the global values it was taken from; a changed
// value means the endpoint was written directly and the ref is dropped.
type selectionRef struct {
	spot slotSpot
	line int
	col  int
}

// Endpoint indexes into selectionState.refs.
const (
	endpointStart = iota
	endpointEnd
	endpointAnchorStart
	endpointAnchorEnd
)

// endpoint returns the global row and column fields of endpoint k.
func (s *selectionState) endpoint(k int) (line, col *int) {
	switch k {
	case endpointStart:
		return &s.startLine, &s.startCol
	case endpointEnd:
		return &s.endLine, &s.endCol
	case endpointAnchorStart:
		return &s.anchorStartLine, &s.anchorStartCol
	default:
		return &s.anchorEndLine, &s.anchorEndCol
	}
}

// dropRefs forgets pending content positions after a direct global write.
func (s *selectionState) dropRefs() { s.refs = [4]*selectionRef{} }

// start initializes a new selection at the given position
func (s *selectionState) start(line, col int) {
	s.dropRefs()
	s.active = true
	s.mouseButtonDown = true
	s.anchored = false
	s.startLine = line
	s.startCol = col
	s.endLine = line
	s.endCol = col
}

// update updates the end position of the selection. While a word anchor is
// set (double-click drag), the selection extends around the anchor instead of
// collapsing to the cursor, so the anchored word always stays selected.
func (s *selectionState) update(line, col int) {
	s.dropRefs()
	if s.anchored {
		switch {
		case line < s.anchorStartLine || (line == s.anchorStartLine && col < s.anchorStartCol):
			s.startLine, s.startCol = s.anchorEndLine, s.anchorEndCol
			s.endLine, s.endCol = line, col
		case line > s.anchorEndLine || (line == s.anchorEndLine && col > s.anchorEndCol):
			s.startLine, s.startCol = s.anchorStartLine, s.anchorStartCol
			s.endLine, s.endCol = line, col
		default:
			s.startLine, s.startCol = s.anchorStartLine, s.anchorStartCol
			s.endLine, s.endCol = s.anchorEndLine, s.anchorEndCol
		}
		return
	}
	s.endLine = line
	s.endCol = col
}

// anchorTo pins the current selection bounds as the anchor that drag
// extension grows around.
func (s *selectionState) anchorTo() {
	s.dropRefs()
	s.anchored = true
	s.anchorStartLine, s.anchorStartCol = s.startLine, s.startCol
	s.anchorEndLine, s.anchorEndCol = s.endLine, s.endCol
}

// end finalizes the selection and stops mouse tracking
func (s *selectionState) end() {
	s.mouseButtonDown = false
}

// clear resets the selection geometry but keeps the multi-click tracking:
// a double-click is a click-release-click sequence whose first release lands
// here, so wiping lastClickTime/clickCount would make multi-click selection
// impossible.
func (s *selectionState) clear() {
	s.dropRefs()
	s.active = false
	s.mouseButtonDown = false
	s.anchored = false
	s.startLine, s.startCol = 0, 0
	s.endLine, s.endCol = 0, 0
	s.mouseY = 0
}

// hasRange reports whether the selection covers at least one character.
func (s *selectionState) hasRange() bool {
	return s.active && (s.startLine != s.endLine || s.startCol != s.endCol)
}

// resetClickTracking forgets the multi-click history so the next press is
// treated as a fresh single click.
func (s *selectionState) resetClickTracking() {
	s.lastClickTime = time.Time{}
	s.clickCount = 0
}

// normalized returns the selection bounds in normalized order (start <= end)
func (s *selectionState) normalized() (startLine, startCol, endLine, endCol int) {
	startLine, startCol = s.startLine, s.startCol
	endLine, endCol = s.endLine, s.endCol

	if startLine > endLine || (startLine == endLine && startCol > endCol) {
		startLine, endLine = endLine, startLine
		startCol, endCol = endCol, startCol
	}
	return startLine, startCol, endLine, endCol
}

// detectClickType records the click and returns the click count (1=single,
// 2=double, 3=triple). Counting cycles after a triple-click so a fourth
// quick click starts a fresh drag selection.
func (s *selectionState) detectClickType(line, col int) int {
	s.dragged = false
	now := time.Now()
	colDiff := col - s.lastClickCol
	isConsecutive := !s.lastClickTime.IsZero() &&
		now.Sub(s.lastClickTime) < styles.DoubleClickThreshold &&
		line == s.lastClickLine &&
		colDiff >= -1 && colDiff <= 1

	if isConsecutive {
		s.clickCount++
		if s.clickCount > 3 {
			s.clickCount = 1
		}
	} else {
		s.clickCount = 1
	}
	s.lastClickTime = now
	s.lastClickLine = line
	s.lastClickCol = col
	return s.clickCount
}

// AutoScrollTickMsg triggers auto-scroll during selection
type AutoScrollTickMsg struct {
	Direction int // -1 for up, 1 for down
}

// DebouncedCopyMsg triggers a debounced copy after multi-click selection
type DebouncedCopyMsg struct {
	ClickID int // Unique identifier to match with current selection state
}

// autoScroll handles automatic scrolling when selecting near viewport edges
func (m *model) autoScroll() tea.Cmd {
	const scrollThreshold = 2
	direction := 0
	var scrollCmd tea.Cmd

	// Selection and edge scrolling share the actual transcript viewport origin.
	viewportY := max(m.selection.mouseY-m.yPos, 0)

	if viewportY < scrollThreshold && m.scrollOffset > 0 {
		// Scroll up - mouse is near top of viewport
		direction = -1
		m.scrollUp()
		// Update endLine to reflect new scroll position
		m.selection.endLine = max(0, m.selection.endLine-1)
	} else if viewportY >= m.height-scrollThreshold {
		// Scroll down - mouse is near or below the bottom of the viewport
		// (drag motion keeps flowing even when the cursor leaves the chat
		// region, e.g. over the editor).
		maxScrollOffset := max(0, m.layoutTotal()-m.height)
		if m.scrollOffset < maxScrollOffset {
			direction = 1
			scrollCmd = m.scrollDown()
			// Update endLine to reflect new scroll position
			m.selection.endLine++
		}
	}

	if direction == 0 {
		return nil
	}

	return tea.Batch(scrollCmd, tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg {
		return AutoScrollTickMsg{Direction: direction}
	}))
}

// selectWordAt selects the word at the given line and column position.
// It reports whether a word was actually selected.
func (m *model) selectWordAt(line, col int) bool {
	m.updateScrollState()
	if line < 0 || line >= m.layoutTotal() {
		return false
	}

	// Keep table separators and borders in place so columns match the screen.
	clusters := lineGraphemes(ansi.Strip(m.renderedLine(line)))
	if len(clusters) == 0 {
		return false
	}

	idx := 0
	for idx < len(clusters)-1 && col >= clusters[idx].endCol {
		idx++
	}
	r, _ := utf8.DecodeRuneInString(clusters[idx].text)
	if boxDrawingChars[r] {
		return false
	}
	onWord, onSpace := isWordChar(r), unicode.IsSpace(r)
	sameClass := func(cluster lineGrapheme) bool {
		r, _ := utf8.DecodeRuneInString(cluster.text)
		return !boxDrawingChars[r] && isWordChar(r) == onWord && unicode.IsSpace(r) == onSpace
	}

	startIdx, endIdx := idx, idx
	for startIdx > 0 && sameClass(clusters[startIdx-1]) {
		startIdx--
	}
	for endIdx < len(clusters)-1 && sameClass(clusters[endIdx+1]) {
		endIdx++
	}
	startCol, endCol := clusters[startIdx].startCol, clusters[endIdx].endCol

	// Set selection
	m.selection.dropRefs()
	m.selection.active = true
	m.selection.anchored = false
	m.selection.startLine = line
	m.selection.startCol = startCol
	m.selection.endLine = line
	m.selection.endCol = endCol
	m.selection.mouseButtonDown = false
	return true
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || r == '_'
}

// selectLineAt selects the entire line at the given line position.
// It reports whether a non-blank line was actually selected.
func (m *model) selectLineAt(line int) bool {
	m.updateScrollState()
	if line < 0 || line >= m.layoutTotal() {
		return false
	}

	originalLine := m.renderedLine(line)
	plainLine := ansi.Strip(originalLine)
	trimmedLine := strings.TrimSpace(plainLine)
	if trimmedLine == "" {
		return false
	}

	// Find start column: position of first non-whitespace character
	startCol := ansi.StringWidth(plainLine) - ansi.StringWidth(strings.TrimLeft(plainLine, " \t"))
	// Find end column: position after last non-whitespace character
	endCol := ansi.StringWidth(strings.TrimRight(plainLine, " \t"))

	// Set selection to cover only the text content (excluding padding/borders)
	m.selection.dropRefs()
	m.selection.active = true
	m.selection.anchored = false
	m.selection.startLine = line
	m.selection.startCol = startCol
	m.selection.endLine = line
	m.selection.endCol = endCol
	m.selection.mouseButtonDown = false
	return true
}

// applySelectionHighlight applies selection highlighting to visible lines
func (m *model) applySelectionHighlight(lines []string, viewportStartLine int) []string {
	startLine, startCol, endLine, endCol := m.selectionBoundsFor(viewportStartLine, viewportStartLine+len(lines))

	highlighted := make([]string, len(lines))

	getLineBounds := func(line string) (textStart, textEnd int) {
		plainLine := ansi.Strip(line)
		textStart = ansi.StringWidth(plainLine) - ansi.StringWidth(strings.TrimLeft(plainLine, " \t"))
		textEnd = ansi.StringWidth(strings.TrimRight(plainLine, " \t"))
		return textStart, textEnd
	}

	for i, line := range lines {
		absoluteLine := viewportStartLine + i

		if absoluteLine < startLine || absoluteLine > endLine {
			highlighted[i] = line
			continue
		}

		textStart, textEnd := getLineBounds(line)
		switch {
		case startLine == endLine && absoluteLine == startLine:
			// Single line selection
			highlighted[i] = m.highlightLine(line, startCol, min(textEnd, endCol))
		case absoluteLine == startLine:
			// Start of multi-line selection
			highlighted[i] = m.highlightLine(line, startCol, textEnd)
		case absoluteLine == endLine:
			// End of multi-line selection
			highlighted[i] = m.highlightLine(line, textStart, min(textEnd, endCol))
		default:
			// Middle of multi-line selection
			highlighted[i] = m.highlightLine(line, textStart, textEnd)
		}
	}

	return highlighted
}

// highlightLine applies selection highlighting to a portion of a line
func (m *model) highlightLine(line string, startCol, endCol int) string {
	return styleLineSegment(line, startCol, endCol, styles.SelectionStyle)
}

// ClearPresentationSelection forgets only transcript selection and capture.
// Keep the copy generation monotonic so an already queued copy cannot act on a
// later selection after the pane becomes active again.
func (m *model) ClearPresentationSelection() {
	oldIndex := m.selectedMessageIndex
	visibleSelection := oldIndex >= 0 || m.selection.hasRange()
	m.selectedMessageIndex = -1
	m.selection = selectionState{pendingCopyID: m.selection.pendingCopyID + 1}
	if oldIndex >= 0 {
		m.invalidateItem(oldIndex)
	}
	// Cancelling capture and queued copies is unconditional, but an idle tab
	// has no selection cells to repaint and must keep its warm transcript.
	if visibleSelection {
		m.renderDirty = true
		m.visualGeneration++
	}
	m.releaseRetainedReplies()
}

// clearSelection resets the selection state
func (m *model) clearSelection() {
	m.cancelImageClick()
	m.selection.clear()
	m.releaseRetainedReplies()
}

// releaseRetainedReplies hands replies kept visible during selection back to
// Work through the normal reply handoff. Call it only after the selection has
// ended. Reader position is left to the existing scroll anchor.
func (m *model) releaseRetainedReplies() {
	if len(m.workRetained) == 0 || m.workRetentionHeld() {
		return
	}
	if !m.workEnabled || m.replay != nil {
		clear(m.workRetained)
		return
	}
	if m.pendingReplyHandoff {
		// The caller's pending snapshot predates this mutation; its completion
		// starts the handoff against the released target.
		clear(m.workRetained)
		m.invalidateTranscriptStructure()
		return
	}
	m.snapshotPendingReplyHandoff(nil)
	clear(m.workRetained)
	m.invalidateTranscriptStructure()
	m.layoutViewport()
	m.beginPendingReplyHandoff()
	m.visualGeneration++
}

// normalizeBounds orders two endpoints, start first.
func normalizeBounds(sl, sc, el, ec int) (int, int, int, int) {
	if sl > el || (sl == el && sc > ec) {
		return el, ec, sl, sc
	}
	return sl, sc, el, ec
}

// selectionTarget is an endpoint located in the current geometry, before any
// slot is measured for the epoch.
type selectionTarget struct {
	k           int
	slot, local int
	line, col   int
}

// selectionTargets locates the endpoints that need a content position for the
// epoch that is starting. Endpoints that already hold a ref keep it while their
// global values still match.
func (m *model) selectionTargets() []selectionTarget {
	s := &m.selection
	if !s.active {
		return nil
	}
	var out []selectionTarget
	for k := range s.refs {
		if (k == endpointAnchorStart || k == endpointAnchorEnd) && !s.anchored {
			continue
		}
		line, col := s.endpoint(k)
		if r := s.refs[k]; r != nil {
			if r.line == *line && r.col == *col {
				continue
			}
			s.refs[k] = nil
		}
		slot, local := m.locate(max(0, *line))
		out = append(out, selectionTarget{k: k, slot: slot, local: local, line: *line, col: *col})
	}
	return out
}

// captureSelectionRefs records the content positions of located endpoints.
// Measuring a slot here is allowed: the old width is still in effect.
func (m *model) captureSelectionRefs(targets []selectionTarget) {
	for _, t := range targets {
		kind := spotStart
		if t.k == endpointEnd || t.k == endpointAnchorEnd {
			kind = spotEnd
		}
		var r selectionRef
		if t.slot >= len(m.views) {
			r.spot = slotSpot{slot: len(m.views), row: t.local, col: t.col}
		} else {
			r.spot = m.spotAt(t.slot, t.local, t.col, kind)
		}
		r.line, r.col = t.line, t.col
		m.selection.refs[t.k] = &r
	}
}

// resolveEndpoint places endpoint k at the current layout. Without materialize,
// an endpoint whose slot is not materialized stays pending.
func (m *model) resolveEndpoint(k int, materialize bool) {
	s := &m.selection
	r := s.refs[k]
	if r == nil {
		return
	}
	line, col := s.endpoint(k)
	if *line != r.line || *col != r.col {
		s.refs[k] = nil
		return
	}
	sp := r.spot
	if sp.slot >= len(m.views) {
		*line, *col = m.geom.Total()+sp.row, sp.col
		s.refs[k] = nil
		return
	}
	if sp.slot < 0 || m.views[sp.slot] != sp.view {
		// The view was replaced: keep the global values rather than guess.
		s.refs[k] = nil
		return
	}
	if !materialize && !m.materialized(sp.slot) {
		return
	}
	m.ensureSlotLines(sp.slot)
	row, c := m.placeSpot(sp)
	*line = m.slotOffset(sp.slot) + row
	*col = c
	s.refs[k] = nil
}

// settleSelection places every pending endpoint, measuring their slots. Copy
// calls it; the copied rows materialize as they are read.
func (m *model) settleSelection() {
	for k := range m.selection.refs {
		m.resolveEndpoint(k, true)
	}
}

// selectionBoundsFor returns the normalized selection for a View of rows
// [top, bottom). Pending endpoints inside the view are placed, which costs
// nothing for rows already on screen. Pending endpoints outside it keep a row
// beyond the view edge, so the highlight is exact without materializing them.
func (m *model) selectionBoundsFor(top, bottom int) (sl, sc, el, ec int) {
	s := &m.selection
	if s.refs[endpointStart] != nil || s.refs[endpointEnd] != nil {
		first := m.slotAt(top)
		last := m.slotAt(max(top, bottom-1))
		sentinel := func(r *selectionRef) int {
			switch {
			case m.geom.position(r.spot.slot) < m.geom.position(first):
				return top - 1
			case m.geom.position(r.spot.slot) > m.geom.position(last):
				return bottom
			default:
				return top
			}
		}
		for _, k := range []int{endpointStart, endpointEnd} {
			if r := s.refs[k]; r != nil && m.geom.position(r.spot.slot) >= m.geom.position(first) && m.geom.position(r.spot.slot) <= m.geom.position(last) {
				m.resolveEndpoint(k, true)
			}
		}
		sl, sc = s.startLine, s.startCol
		el, ec = s.endLine, s.endCol
		if r := s.refs[endpointStart]; r != nil {
			sl, sc = sentinel(r), 0
		}
		if r := s.refs[endpointEnd]; r != nil {
			el, ec = sentinel(r), 0
		}
		return normalizeBounds(sl, sc, el, ec)
	}
	return normalizeBounds(s.startLine, s.startCol, s.endLine, s.endCol)
}

// setSelectionSpan sets a plain selection between two global cells. It is the
// geometry-only write that mouse handling uses; it drops pending content refs.
func (m *model) setSelectionSpan(startLine, startCol, endLine, endCol int) {
	m.selection.dropRefs()
	m.selection.active = true
	m.selection.anchored = false
	m.selection.mouseButtonDown = false
	m.selection.startLine, m.selection.startCol = startLine, startCol
	m.selection.endLine, m.selection.endCol = endLine, endCol
}

// selectSourceSpan selects by content: from source byte startSrc of view
// startView to the end of the last matched cell before endSrc in view endView.
// Both views are measured at the current width. It returns false when either
// source offset has no matched cell.
//
// The global cells are only correct for the offsets measured now: an unmeasured
// slot between the views, or above them, can still change height. Each endpoint
// therefore also keeps its source offset as a content ref, placed again when the
// selection settles (copy, highlight), so the span follows its content.
func (m *model) selectSourceSpan(startView, startSrc, endView, endSrc int) bool {
	// The span's slots, including the answers between its ends, are measured
	// first so the offsets read below are exact for the span. Slots outside the
	// span are not touched.
	for i := max(0, startView); i <= min(endView, len(m.views)-1); i++ {
		m.ensureSlotLines(i)
	}
	startLine, startCol, startSpot, ok := m.sourceEndpoint(startView, startSrc, spotStart)
	if !ok {
		return false
	}
	endLine, endCol, endSpot, ok := m.sourceEndpoint(endView, endSrc, spotEnd)
	if !ok {
		return false
	}
	m.setSelectionSpan(startLine, startCol, endLine, endCol)
	m.selection.refs[endpointStart] = &selectionRef{spot: startSpot, line: startLine, col: startCol}
	m.selection.refs[endpointEnd] = &selectionRef{spot: endSpot, line: endLine, col: endCol}
	return true
}

// sourceEndpoint returns the global row and column of a source offset in view,
// and the content position of that offset. Start offsets use the first matched
// cell at or after src; end offsets use the cell after the last matched cell
// that ends at or before src. The view is materialized at the current width.
func (m *model) sourceEndpoint(view, src int, kind spotKind) (line, col int, sp slotSpot, ok bool) {
	if view < 0 || view >= len(m.views) {
		return 0, 0, slotSpot{}, false
	}
	m.ensureSlotLines(view)
	lines := m.slotLines(view)
	sm := BuildSourceMap(m.slotSourceText(view), lines)
	var row int
	if kind == spotStart {
		row, col, ok = sm.ColForSource(src)
	} else {
		row, col, ok = sm.CellAfterSource(src)
	}
	if !ok {
		return 0, 0, slotSpot{}, false
	}
	sp = slotSpot{view: m.views[view], slot: view, kind: kind, mode: anchorSource, hasSrc: true, src: src, rows: len(lines)}
	return m.slotOffset(view) + row, col, sp, true
}
