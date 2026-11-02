package messages

import (
	"slices"
	"strings"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Viewport anchor across width and theme epochs.
//
// Capture runs before the epoch invalidates geometry and records the reader's
// top row as a slotSpot: the view, the row, and for textual content the source
// byte offset of the first matched cell plus the rows between the top row and
// that cell. Restore runs in layoutViewport before the visible walk: it measures
// the anchored slot at the new width, maps the source offset back to a row, and
// sets scrollOffset. Between epochs the top row is re-recorded as (view, local
// row), so measured corrections above the reader keep the same content on the
// same screen row. Every step is O(log N) plus one slot's source map.

// anchorCounters tallies how positions resolved. Textual positions that could
// not be proven fall back to row 0; that is a failure on real content.
type anchorCounters struct {
	source   atomic.Uint64 // textual positions placed by source offset
	opaque   atomic.Uint64 // opaque positions placed proportionally
	rows     atomic.Uint64 // positions without source text, placed by row index
	unproven atomic.Uint64 // textual positions with no proof, placed at row 0
}

// viewportAnchorStats is process-wide; tests read deltas.
var viewportAnchorStats anchorCounters

// anchorMode says how a position inside one slot crosses an epoch.
type anchorMode uint8

const (
	anchorRows   anchorMode = iota // no source text: same row index, clamped
	anchorOpaque                   // opaque by message type: ProportionalRow
	anchorSource                   // textual: source offset through SourceMap
)

// spotKind selects the mapping rule for a textual position.
type spotKind uint8

const (
	spotTop   spotKind = iota // top visible row: first matched cell at or after it
	spotStart                 // selection start: first matched cell at or after the cell
	spotEnd                   // selection end: cell after the last matched cell before the cell
)

// slotSpot is a position inside one slot that survives a width epoch.
type slotSpot struct {
	view     layout.Model
	slot     int
	kind     spotKind
	mode     anchorMode
	hasSrc   bool
	src      int // source byte offset; for spotEnd the exclusive end
	lead     int // display columns from the matched cell to the recorded column
	residual int // spotTop: rows from the top row to the row holding src
	row, col int // position inside the slot at capture
	rows     int // slot rows at capture (opaque scaling)
}

// anchorFollowsContentAbove decides what the settled top row does when real
// content above it changes size, such as a tool card expanding above a
// scrolled-up reader. false (base behaviour, pending a user decision): the
// numeric scroll is kept, so that content moves on screen as before. true: the
// reader's top row stays on its content. Estimate corrections and width or
// theme epochs stay anchored either way. settleViewportAnchor is the only
// place that reads this flag.
var anchorFollowsContentAbove = false

// viewportAnchor is the reader's settled top row and the pending epoch capture.
type viewportAnchor struct {
	valid   bool
	slot    int
	view    layout.Model
	local   int // row inside slot at the top of the viewport
	top     int // scrollOffset when this record was taken
	pending bool
	spot    slotSpot // valid while pending
	// notes are the content slots above the settled row that changed since the
	// reader last scrolled, with the height each had before its change. Only
	// their measured height changes are content, never estimate corrections.
	notes []contentNote
	// cell is the source offset of the exact content the reader was anchored
	// to, kept while the reader does not scroll. It stops the anchor from
	// sliding to the start of the wrapped row at each epoch.
	cell    int
	hasCell bool
}

// contentNote is one content change above the settled row: the slot and the
// height it had before the change.
type contentNote struct {
	index int
	base  int
}

// noteAnchorContentChange is called before the content of view index changes
// (not only its estimate). A change above the settled top row records the
// slot's current height, so settleViewportAnchor can tell it from estimates.
func (m *model) noteAnchorContentChange(index int) {
	a := &m.layoutAnchor
	if !a.valid || index < 0 || index >= a.slot || index >= len(m.slotBase) {
		return
	}
	for _, n := range a.notes {
		if n.index == index {
			return
		}
	}
	a.notes = append(a.notes, contentNote{index: index, base: m.slotBase[index]})
}

// opaqueSlot reports whether slot i is opaque by its message type: image or
// media content with no text. Text is never inferred from match results.
func (m *model) opaqueSlot(i int) bool {
	if _, grouped := m.workHeaderBySource[i]; grouped {
		return false
	}
	if i < 0 || i >= len(m.messages) {
		return false
	}
	msg := m.messages[i]
	if len(msg.Images) == 0 && len(msg.AssistantMedia) == 0 {
		return false
	}
	return strings.TrimSpace(anchorSourceText(msg)) == ""
}

// anchorSourceText is the raw text the message renders from.
func anchorSourceText(msg *types.Message) string {
	if msg.DisplayContent != "" {
		return msg.DisplayContent
	}
	return copyableMessageContent(msg)
}

// slotSourceText returns the raw text of slot i. A work block is the
// concatenation of its child messages in order, so its rows map to one source.
func (m *model) slotSourceText(i int) string {
	if _, grouped := m.workHeaderBySource[i]; grouped {
		var b strings.Builder
		for _, item := range m.workItemsForAnchor(i) {
			if item.SourceIndex >= 0 && item.SourceIndex < len(m.messages) {
				b.WriteString(anchorSourceText(m.messages[item.SourceIndex]))
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	if i < 0 || i >= len(m.messages) {
		return ""
	}
	return anchorSourceText(m.messages[i])
}

// slotLines returns the materialized rows of slot i at the current width.
func (m *model) slotLines(i int) []string {
	e, ok := m.entryFor(i)
	if !ok {
		return nil
	}
	if e.virtual {
		if e.seg == nil {
			return nil
		}
		return slices.Concat(e.seg.Header, e.seg.Stable, e.seg.Tail)
	}
	return e.lines
}

// ensureSlotLines materializes slot i at the current width if needed.
func (m *model) ensureSlotLines(i int) {
	if !m.materialized(i) {
		m.measureSlot(i)
	}
}

// locate returns the slot holding global row g and the row inside it. Rows at
// or past the transcript end locate to len(views) with the offset past the end.
// Callers locate every position before measuring any slot, because measuring
// moves the offsets of the slots after it.
func (m *model) locate(g int) (slot, local int) {
	slot = m.slotAt(g)
	return slot, g - m.slotOffset(slot)
}

// spotAt records the position (row, col) inside slot at the current width.
// It measures the slot when needed; locate every position first. For spotTop,
// a cell that still lies on row is kept as the anchor instead of the row start.
func (m *model) spotAt(slot, row, col int, kind spotKind, cell ...int) slotSpot {
	sp := slotSpot{view: m.views[slot], slot: slot, kind: kind, row: row, col: col}
	m.ensureSlotLines(slot)
	lines := m.slotLines(slot)
	sp.rows = len(lines)
	if m.opaqueSlot(slot) {
		sp.mode = anchorOpaque
		return sp
	}
	text := m.slotSourceText(slot)
	if strings.TrimSpace(text) == "" || row < 0 || row >= len(lines) {
		sp.mode = anchorRows
		return sp
	}
	sm := BuildSourceMap(text, lines)
	sp.mode = anchorSource
	if sm.Status() == MapUnproven {
		// Text on one side only: no proof. Restore counts it as a failure.
		return sp
	}
	switch kind {
	case spotTop:
		if len(cell) > 0 {
			if r, ok := sm.RowForSource(cell[0]); ok && r == row {
				sp.hasSrc, sp.src = true, cell[0]
				break
			}
		}
		if src, ok := sm.TopAnchor(row); ok {
			r, _ := sm.RowForSource(src)
			sp.hasSrc, sp.src, sp.residual = true, src, r-row
		}
	case spotStart:
		if src, _, mc, ok := sm.StartAt(row, col); ok {
			sp.hasSrc, sp.src, sp.lead = true, src, col-mc
		}
	case spotEnd:
		if src, _, ec, ok := sm.EndBefore(row, col); ok {
			sp.hasSrc, sp.src, sp.lead = true, src, col-ec
		}
	}
	if !sp.hasSrc {
		// Decoration with no matched content on this side: keep the row.
		sp.mode = anchorRows
	}
	return sp
}

// placeSpot returns the row and column of sp at the current width. The slot
// must be materialized.
func (m *model) placeSpot(sp slotSpot) (row, col int) {
	lines := m.slotLines(sp.slot)
	n := len(lines)
	if n == 0 {
		return 0, 0
	}
	switch sp.mode {
	case anchorOpaque:
		viewportAnchorStats.opaque.Add(1)
		return ProportionalRow(sp.row, sp.rows, n), sp.col
	case anchorRows:
		viewportAnchorStats.rows.Add(1)
		return min(max(sp.row, 0), n-1), sp.col
	}
	sm := BuildSourceMap(m.slotSourceText(sp.slot), lines)
	if !sp.hasSrc || sm.Status() == MapUnproven {
		viewportAnchorStats.unproven.Add(1)
		return 0, 0
	}
	viewportAnchorStats.source.Add(1)
	switch sp.kind {
	case spotTop:
		r, ok := sm.RowForSource(sp.src)
		if !ok {
			viewportAnchorStats.unproven.Add(1)
			return 0, 0
		}
		return min(max(r-sp.residual, 0), n-1), 0
	case spotStart:
		r, c, ok := sm.ColForSource(sp.src)
		if !ok {
			viewportAnchorStats.unproven.Add(1)
			return 0, 0
		}
		return r, max(0, c+sp.lead)
	default:
		r, c, ok := sm.CellAfterSource(sp.src)
		if !ok {
			viewportAnchorStats.unproven.Add(1)
			return 0, 0
		}
		return r, max(0, c+sp.lead)
	}
}

// captureViewportAnchor records the reader's top row before a width or theme
// epoch, together with the selection endpoints. The old geometry and lines are
// still valid here. An earlier pending capture is kept: its positions were
// taken at the width before the epoch.
func (m *model) captureViewportAnchor() {
	m.syncSlots()
	if len(m.views) == 0 {
		return
	}
	// An active reply handoff places the reader itself (restoreReplyHandoffAnchor).
	wantAnchor := !m.layoutAnchor.pending && m.userHasScrolled && !m.replyHandoff.Active()
	var anchorSlot, anchorLocal int
	if wantAnchor {
		anchorSlot, anchorLocal = m.locate(m.scrollOffset)
	}
	targets := m.selectionTargets()
	if wantAnchor && anchorSlot < len(m.views) {
		var spot slotSpot
		if a := m.layoutAnchor; a.valid && a.hasCell && a.top == m.scrollOffset && a.slot == anchorSlot {
			spot = m.spotAt(anchorSlot, anchorLocal, 0, spotTop, a.cell)
		} else {
			spot = m.spotAt(anchorSlot, anchorLocal, 0, spotTop)
		}
		m.layoutAnchor = viewportAnchor{
			valid: true, slot: anchorSlot, view: m.views[anchorSlot],
			local: anchorLocal, top: m.scrollOffset, pending: true, spot: spot,
		}
	}
	m.captureSelectionRefs(targets)
}

// restoreViewportAnchor places the pending epoch capture at the new width, or
// keeps a settled top row on its content when slots above it were corrected.
// It runs in layoutViewport before the visible walk.
func (m *model) restoreViewportAnchor() {
	a := &m.layoutAnchor
	if !a.pending {
		m.settleViewportAnchor()
		return
	}
	a.pending = false
	a.notes = nil
	sp := a.spot
	a.hasCell = false
	if m.replyHandoff.Active() {
		// The reply handoff owns the reader's place for this epoch.
		a.valid = false
		return
	}
	if m.scrollOffset != a.top {
		// The reader placed the viewport after the epoch started: that position wins.
		a.valid = false
		m.recordAnchorTop()
		return
	}
	if sp.slot < 0 || sp.slot >= len(m.views) || m.views[sp.slot] != sp.view {
		a.valid = false
		return
	}
	m.ensureSlotLines(sp.slot)
	row, _ := m.placeSpot(sp)
	before := m.scrollOffset
	m.scrollOffset = m.slotOffset(sp.slot) + row
	m.consumeGeometryDelta(m.scrollOffset - before)
	m.scrollview.SetLayoutScrollOffset(m.scrollOffset)
	m.recordAnchorTop()
	if sp.mode == anchorSource && sp.hasSrc && sp.kind == spotTop {
		a.cell, a.hasCell = sp.src, true
	}
}

// settleViewportAnchor keeps the reader's top row on its content when the
// geometry above it changed (an estimate corrected or content resized above)
// without the scroll position changing. Pinned bottom-follow never anchors.
func (m *model) settleViewportAnchor() {
	a := &m.layoutAnchor
	if !m.userHasScrolled || len(m.views) == 0 {
		a.valid = false
		a.notes = nil
		return
	}
	if !a.valid || a.top != m.scrollOffset {
		// The reader moved (or nothing is settled yet): start a new record.
		a.notes = nil
		m.recordAnchorTop()
		return
	}
	if a.slot < len(m.views) && m.views[a.slot] == a.view && !m.replyHandoff.Active() {
		// Measured content changes above the reader are the notes whose height
		// moved. Notes not yet measured stay pending.
		content := 0
		kept := a.notes[:0]
		for _, n := range a.notes {
			switch {
			case n.index >= len(m.slotBase):
			case m.slotBase[n.index] != n.base:
				content += m.slotBase[n.index] - n.base
			default:
				kept = append(kept, n)
			}
		}
		a.notes = kept
		want := m.slotOffset(a.slot) + a.local
		if !anchorFollowsContentAbove {
			want -= content
		}
		want = max(0, want)
		if want != m.scrollOffset {
			old := m.scrollOffset
			m.scrollOffset = want
			// Only the shift this path applies is consumed; a content term left out
			// above (switch off) stays unconsumed.
			m.consumeGeometryDelta(want - old)
			m.scrollview.SetLayoutScrollOffset(m.scrollOffset)
		}
	}
	m.recordAnchorTop()
}

// recordAnchorTop settles the top row at the current scroll position.
func (m *model) recordAnchorTop() {
	a := &m.layoutAnchor
	slot := m.slotAt(m.scrollOffset)
	if slot >= len(m.views) {
		a.valid = false
		return
	}
	a.valid = true
	a.slot = slot
	a.view = m.views[slot]
	a.local = m.scrollOffset - m.slotOffset(slot)
	a.top = m.scrollOffset
}
