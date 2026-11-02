package messages

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Slot layout. Every top-level view is one slot. The Fenwick index (geom)
// holds each slot's height: its last measured height, or a retained estimate
// when the slot has not been measured since the last epoch. A materialized
// slot has its lines in renderedItems; only materialized slots are written
// into the renderedLines mirror.
//
// Work children are hidden slots with zero height. Their line offset is the
// block offset plus the child's row start inside the block.

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// hiddenSlot reports whether slot i is a work child shown inside its block.
func (m *model) hiddenSlot(i int) bool {
	if _, ok := m.workSourceToHeader[i]; !ok {
		return false
	}
	return m.workHeaderBySource[i] == nil
}

// materialized reports whether slot i has its lines in renderedItems.
func (m *model) materialized(i int) bool {
	_, ok := m.entryFor(i)
	return ok
}

// entryFor returns the materialized entry of slot i. Cacheable entries live in
// renderedItems (the historical cache); the rest live in uncached.
func (m *model) entryFor(i int) (renderedItemRange, bool) {
	if e, ok := m.renderedItems[i]; ok {
		return e, true
	}
	e, ok := m.uncached[i]
	return e, ok
}

// setEntry stores the freshly measured entry of slot i. The slot's previous
// rows are cleared first, so a slot never owns rows twice.
func (m *model) setEntry(i int, e renderedItemRange) {
	m.dropEntry(i)
	if e.cacheable {
		m.renderedItems[i] = e
	} else {
		if m.uncached == nil {
			m.uncached = make(renderedItemIndex)
		}
		m.uncached[i] = e
	}
	m.entryHigh = max(m.entryHigh, i)
	m.markFacade()
}

// dropEntry forgets slot i's lines and clears the rows they occupy. The height
// estimate stays in the index.
func (m *model) dropEntry(i int) {
	e, ok := m.entryFor(i)
	if !ok {
		return
	}
	m.clearRows(e)
	delete(m.renderedItems, i)
	delete(m.uncached, i)
	if m.activeSegments != nil && m.activeSegments.index == i {
		m.activeSegments = nil
	}
	m.urlSpans.clear()
}

// dropSlot forgets slot i's lines and marks its height unknown: the content may
// have changed, so the slot is measured again when the next layout pass runs.
func (m *model) dropSlot(i int) {
	m.dropEntry(i)
	if i >= 0 && i < len(m.slotKnown) && m.slotKnown[i] {
		m.slotKnown[i] = false
		m.slotPending = append(m.slotPending, i)
	}
}

// replaceView stores a forwarded view update. Only a changed view object
// alters the slot structure.
func (m *model) replaceView(i int, old, updated layout.Model) {
	m.views[i] = updated
	if updated != old {
		m.noteReplacedView(i)
	}
}

// noteReplacedView records that slot i holds a different view object. The
// layout index repairs that one slot instead of rebuilding every slot.
func (m *model) noteReplacedView(i int) {
	m.slotReplaced = append(m.slotReplaced, i)
}

// settleGeometry brings the slot index up to date and measures slots whose
// height is unknown. It never measures known slots, so it is cheap on a
// steady transcript and renders only what changed.
func (m *model) settleGeometry() {
	m.syncSlots()
	m.measurePending()
	m.publishFacade(false)
}

// clearRows empties the rows that entry e last wrote into renderedLines. It
// maintains only the compatibility facade backing/length; never read for layout
// decisions; called only from publishFacade, dropEntry and dropMaterialized.
func (m *model) clearRows(e renderedItemRange) {
	// The whole backing array is searched: rows may lie beyond the current length
	// after a shrink, and they must still read empty when the length grows again.
	buf := m.renderedLines[:cap(m.renderedLines)]
	if e.span > 0 && e.start >= 0 && e.start+e.span <= len(buf) {
		clear(buf[e.start : e.start+e.span])
		m.stats.rowsCleared += uint64(e.span)
	}
}

// markFacade records that the renderedLines mirror must be rewritten.
func (m *model) markFacade() {
	m.facadeDirty = true
	m.layoutFull = false
}

// cacheableSlot reports whether slot i's lines belong in the historical cache.
func (m *model) cacheableSlot(i int) bool {
	return m.shouldCacheMessage(i) && (!m.focused || i != m.selectedMessageIndex) && i != m.hoveredMessageIndex && m.hoverActionProgress(i) == 0
}

// markSlotsStale marks the slot structure stale after a same-length reorder
// or in-place view replacement. Length changes are detected by syncSlots.
func (m *model) markSlotsStale() { m.slotsStale = true }

// syncSlots makes the index match m.views. Appends, tail truncations and
// same-length view replacements are repaired incrementally, each in O(log N)
// per changed slot. Anything else (a reorder or an initial load) rebuilds the
// index in O(N) through syncSlotsFull.
func (m *model) syncSlots() {
	n := len(m.views)
	oldN := len(m.slotViews)
	if m.slotsStale || oldN == 0 || m.geom.Len() != oldN || len(m.slotBase) != oldN || len(m.slotKnown) != oldN || len(m.slotSep) != oldN || len(m.slotTheme) != oldN {
		m.syncSlotsFull()
		return
	}
	if n == oldN && len(m.slotReplaced) == 0 {
		return
	}
	for _, i := range m.slotReplaced {
		if i < oldN && i < n && m.slotViews[i] != m.views[i] {
			m.replaceSlot(i)
		}
	}
	m.slotReplaced = m.slotReplaced[:0]
	switch {
	case n > oldN:
		m.appendSlots(oldN, n)
	case n < oldN:
		m.truncateSlots(n)
	}
}

// slotHeightSeed is the height a never-measured slot starts with, and whether
// it is already known (history not yet paged in and work children are zero).
func (m *model) slotHeightSeed(i int) (base int, known bool) {
	switch {
	case m.hiddenSlot(i):
		return 0, true
	case i < m.historyStart:
		return 0, false
	default:
		return m.placeholderRows(i), false
	}
}

// placeholderRows estimates the rows of a slot that has not been measured yet
// from its content length at the current width. It is O(1) per item and never
// renders: a placeholder that is close to the real height keeps the total
// close to exact until the slot is shown and measured.
func (m *model) placeholderRows(i int) int {
	// Separator and the message's own padding rows, which the text does not count.
	const chrome = 2
	if i < 0 || i >= len(m.messages) || m.messages[i] == nil {
		return 1
	}
	width := max(1, m.contentWidth())
	return chrome + max(1, (len(m.messages[i].Content)+width-1)/width)
}

// replaceSlot gives slot i a new view object: its entry and height are reset to
// a placeholder that the next layout pass measures.
func (m *model) replaceSlot(i int) {
	m.dropEntry(i)
	m.slotViews[i] = m.views[i]
	m.slotTheme[i] = styles.ThemeGeneration()
	base, known := m.slotHeightSeed(i)
	m.slotBase[i], m.slotKnown[i] = base, known
	m.geom.Set(i, base+b2i(m.slotSep[i]))
	if !known && i >= m.historyStart {
		m.slotPending = append(m.slotPending, i)
	}
	m.markFacade()
}

// appendSlots adds the views [oldN, n) at the tail. The previous last slot's
// separator may change, so separators are refreshed from there on.
func (m *model) appendSlots(oldN, n int) {
	for i := oldN; i < n; i++ {
		base, known := m.slotHeightSeed(i)
		m.slotViews = append(m.slotViews, m.views[i])
		m.slotBase = append(m.slotBase, base)
		m.slotKnown = append(m.slotKnown, known)
		m.slotSep = append(m.slotSep, false)
		m.slotTheme = append(m.slotTheme, styles.ThemeGeneration())
		m.geom.Append(base)
		if !known && i >= m.historyStart {
			m.slotPending = append(m.slotPending, i)
		}
	}
	m.refreshSeparators(max(oldN-1, 0), n)
	if oldN > 0 {
		if e, ok := m.entryFor(oldN - 1); ok && e.virtual {
			// A segmented entry is only valid for the last slot.
			m.dropEntry(oldN - 1)
		}
	}
	m.markFacade()
}

// truncateSlots drops the views at and after n.
func (m *model) truncateSlots(n int) {
	for i := n; i < len(m.slotViews); i++ {
		m.dropEntry(i)
	}
	m.geom.Truncate(n)
	m.slotViews = m.slotViews[:n]
	m.slotBase = m.slotBase[:n]
	m.slotKnown = m.slotKnown[:n]
	m.slotSep = m.slotSep[:n]
	m.slotTheme = m.slotTheme[:n]
	m.refreshSeparators(max(n-1, 0), n)
	m.markFacade()
}

// refreshSeparators recomputes separator rows for slots [from, to).
func (m *model) refreshSeparators(from, to int) {
	for i := from; i < to; i++ {
		sep := m.displayNeedsSeparator(i)
		if sep != m.slotSep[i] {
			m.slotSep[i] = sep
			m.geom.Set(i, m.slotBase[i]+b2i(sep))
		}
	}
}

func (m *model) syncSlotsFull() {
	m.stats.slotRebuilds++
	m.stats.slotLoops += uint64(len(m.views))
	n := len(m.views)
	if !m.slotsStale && m.geom.Len() == n && len(m.slotBase) == n {
		return
	}
	m.slotsStale = false
	m.slotReplaced = m.slotReplaced[:0]
	oldN := len(m.slotViews)
	prev := make(map[layout.Model]int, oldN)
	for i, view := range m.slotViews {
		prev[view] = i
	}
	oldBase, oldSep, oldTheme := m.slotBase, m.slotSep, m.slotTheme
	gen := styles.ThemeGeneration()
	theme := make([]uint64, n)
	base := make([]int, n)
	known := make([]bool, n)
	matched := make([]int, n)
	for i, view := range m.views {
		matched[i] = -1
		if j, ok := prev[view]; ok && j < len(oldBase) {
			base[i] = oldBase[j]
			known[i] = m.slotKnown[j]
			theme[i] = oldTheme[j]
			matched[i] = j
		} else {
			theme[i] = gen
			// Never measured: a content-sized placeholder until the slot is measured.
			base[i] = m.placeholderRows(i)
		}
	}
	// Entries are keyed by slot index: an entry whose view moved (insert,
	// remove, replace) is no longer its slot's lines.
	for i := range n {
		if i >= oldN || matched[i] != i {
			m.dropEntry(i)
		}
	}
	for i := n; i < oldN; i++ {
		m.dropEntry(i)
	}
	// A segmented (virtual) entry is only valid for the last slot. The previous
	// last slot stops being it when a view is appended, so it is measured again.
	if oldN > 0 && oldN-1 < n {
		if e, ok := m.entryFor(oldN - 1); ok && e.virtual {
			m.dropEntry(oldN - 1)
			m.markFacade()
		}
	}
	m.slotViews = slices.Clone(m.views)
	m.slotSep = make([]bool, n)
	vals := make([]int, n)
	from := n
	for i := range n {
		// An entry describes one role: a work block anchor, a child of a block,
		// or a plain item. A slot whose role changed is measured again.
		if e, ok := m.entryFor(i); ok && (m.hiddenSlot(i) || e.work != (m.workHeaderBySource[i] != nil)) {
			m.dropEntry(i)
			known[i] = false
		}
		if i < m.historyStart {
			// History not yet paged in: zero height until ContinueHistory adds it.
			base[i] = 0
			known[i] = false
		}
		if m.hiddenSlot(i) {
			base[i] = 0
			known[i] = true
		}
		m.slotSep[i] = m.displayNeedsSeparator(i)
		vals[i] = base[i] + b2i(m.slotSep[i])
		if i >= oldN || matched[i] != i || base[i] != oldBase[i] || m.slotSep[i] != oldSep[i] {
			from = min(from, i)
		}
		if i >= m.historyStart && !known[i] && !m.hiddenSlot(i) {
			m.slotPending = append(m.slotPending, i)
		}
	}
	m.slotBase = base
	m.slotKnown = known
	m.slotTheme = theme
	m.geom.Rebuild(vals)
	if from < n {
		m.markFacade()
	}
}

// measurePending measures every placeholder slot that the viewport layout is
// about to depend on, so the total is exact after each View. Only never-measured
// slots are touched: a steady-state View does no measuring.
func (m *model) measurePending() {
	// Work blocks are not polled here: every path that changes a block's rows or
	// height drops its anchor slot (invalidateItemWith, invalidateWorkBlock,
	// syncWorkProjection). Polling Height() would render every block on every pass.
	// Placeholders are measured by the viewport loop only (viewport + fill), so
	// an initial load renders what is shown and estimates the rest.
	m.slotPending = m.slotPending[:0]
}

// dropMaterialized clears every materialized row and forgets every
// materialized slot. Heights and offsets are kept as estimates.
func (m *model) dropMaterialized() {
	for i, e := range m.renderedItems {
		m.clearRows(e)
		delete(m.renderedItems, i)
	}
	for i, e := range m.uncached {
		m.clearRows(e)
		delete(m.uncached, i)
	}
	m.entryHigh = -1
	m.activeSegments = nil
	m.workChildLocal = nil
	m.markFacade()
	m.layoutFull = false
	m.urlSpans.clear()
}

// slotOffset returns the first global line of view i. Work children are
// placed inside their block.
func (m *model) slotOffset(i int) int {
	if m.legacyFacadeMode() {
		return m.legacyOffset(i)
	}
	m.syncSlots()
	if i < 0 {
		return 0
	}
	if i >= m.geom.Len() {
		return m.geom.Total()
	}
	if m.hiddenSlot(i) {
		anchor := m.workSourceToHeader[i]
		if anchor >= 0 && anchor < m.geom.Len() {
			if local, ok := m.workChildLocal[i]; ok {
				return m.geom.Offset(anchor) + local
			}
		}
	}
	return m.geom.Offset(i)
}

// slotAt returns the view index whose rows contain global line g, or
// len(views) when g is at or past the transcript end.
func (m *model) slotAt(g int) int {
	m.syncSlots()
	return m.geom.Find(g)
}

// setSlotBase records a new item height for slot i (separator excluded).
func (m *model) setSlotBase(i, h int) {
	m.slotKnown[i] = true
	if m.slotBase[i] == h {
		return
	}
	m.slotBase[i] = h
	m.geom.Set(i, h+b2i(m.slotSep[i]))
	m.markFacade()
}

// measureSlot renders slot i and makes its lines and height the current
// ones. Only the lines of this slot are produced; neighbours are untouched.
func (m *model) measureSlot(i int) {
	if i < 0 || i >= len(m.views) || m.hiddenSlot(i) || i < m.historyStart {
		return
	}
	m.stats.itemVisits++
	if block := m.workHeaderBySource[i]; block != nil {
		m.measureWorkBlock(i, block)
		return
	}
	m.syncViewTheme(i)
	m.sizeSlotView(i)
	item := m.renderItem(i, m.views[i])
	cacheable := m.cacheableSlot(i)
	if item.segments != nil && i == len(m.views)-1 {
		h := len(item.segments.Header) + len(item.segments.Stable) + len(item.segments.Tail)
		m.setSlotBase(i, h)
		m.setEntry(i, renderedItemRange{height: h, virtual: true, seg: item.segments, cacheable: cacheable})
		return
	}
	lines := m.renderedItemLines(item)
	m.setSlotBase(i, len(lines))
	m.setEntry(i, renderedItemRange{height: len(lines), lines: lines, cacheable: cacheable})
}

// sizeSlotView gives slot i's view the width it renders at now: a work child is
// sized inside its block's inset. A resize only changes the width used here, so
// an unmeasured view keeps its old width until the viewport reaches it.
func (m *model) sizeSlotView(i int) {
	width := m.contentWidth()
	if _, grouped := m.workSourceToHeader[i]; grouped {
		width = workblock.SourceContentWidth(width)
	}
	view := m.views[i]
	if child, ok := view.(interface{ GetSize() (int, int) }); ok {
		if current, _ := child.GetSize(); current == width {
			return
		}
	}
	m.stats.sizeCalls++
	view.SetSize(width, 0)
}

// syncViewTheme restyles slot i's view when the theme changed since the slot last
// rendered. A theme change never walks the transcript: only the slots that are
// measured afterwards pay for their cache invalidation, so the cost is bounded by
// what the viewport shows. Reasoning blocks check the theme generation themselves
// when they render, so only message views need an explicit invalidation here.
func (m *model) syncViewTheme(i int) {
	if i < 0 || i >= len(m.slotTheme) || i >= len(m.views) {
		return
	}
	gen := styles.ThemeGeneration()
	if m.slotTheme[i] == gen {
		return
	}
	m.slotTheme[i] = gen
	m.stats.themeUpdates++
	if mv, ok := m.views[i].(message.Model); ok {
		m.stats.viewCacheInvalidations++
		mv.InvalidateRenderCache()
	}
}

// measureWorkBlock renders a work block as one slot and records the row start
// of each child inside the block.
func (m *model) measureWorkBlock(anchor int, block *workblock.Model) {
	gen := styles.ThemeGeneration()
	// The block's own header and body are theme-dependent: a block last rendered
	// under another theme is invalidated before it renders again.
	if anchor >= 0 && anchor < len(m.slotTheme) && m.slotTheme[anchor] != gen {
		block.Invalidate()
	}
	items := m.workItemsForAnchor(anchor)
	for _, item := range items {
		m.syncViewTheme(item.SourceIndex)
	}
	if anchor >= 0 && anchor < len(m.slotTheme) {
		m.slotTheme[anchor] = gen
	}
	block.SetItems(items)
	block.SetSize(m.contentWidth(), 0)
	lines := splitRendered(m.blockView(block))
	m.setSlotBase(anchor, m.blockHeight(block))
	m.setEntry(anchor, renderedItemRange{height: len(lines), lines: lines, work: true})
	for _, row := range m.blockRows(block) {
		if row.SourceIndex < 0 || row.SourceIndex == anchor || row.SourceIndex >= len(m.views) {
			continue
		}
		if m.workChildLocal == nil {
			m.workChildLocal = make(map[int]int)
		}
		m.workChildLocal[row.SourceIndex] = row.Start
	}
}

// syncFacade rewrites the renderedLines mirror for materialized slots at their
// current offsets. Cost is proportional to the materialized rows, never to the
// whole history; the backing array is reused and only grows geometrically.
// publishFacade is the only writer of the compatibility facade (renderedLines,
// lineOffsets, totalHeight, activeSegments.start) outside the explicit
// legacy-mode and reset paths. It derives every facade field from one authority
// snapshot and decides nothing: it does not cancel hover, and it never walks
// history. Rows are written for materialized entries and the bottom window only.
// Length follows the authority total on a normal publish (tail append) but never
// grows during a width or theme epoch, so an epoch allocates nothing that scales
// with history; ensureAllItemsRendered passes full to demand every row.
func (m *model) publishFacade(full bool) {
	if !m.facadeDirty && !full {
		return
	}
	m.facadeDirty = false
	n := len(m.views)
	for len(m.lineOffsets) < n {
		m.lineOffsets = append(m.lineOffsets, 0)
	}
	if len(m.lineOffsets) > n {
		m.lineOffsets = m.lineOffsets[:n]
	}
	// Rows are written only by materialized entries, so clearing exactly those
	// spans leaves the whole mirror empty. No slot index is walked.
	for i, e := range m.renderedItems {
		m.stats.facadeEntries++
		if e.span > 0 {
			m.clearRows(e)
			e.span = 0
			m.renderedItems[i] = e
		}
	}
	for i, e := range m.uncached {
		if e.span > 0 {
			m.clearRows(e)
			e.span = 0
			m.uncached[i] = e
		}
	}
	total := m.geom.Total()
	size := total
	var active *activeTranscriptSegments
	if n > 0 {
		if e, ok := m.entryFor(n - 1); ok && e.virtual && e.seg != nil {
			size = m.geom.Offset(n - 1)
			if a := m.activeSegments; a != nil && a.index == n-1 && m.activeSegSource == e.seg {
				a.start = size
				active = a
			} else {
				active = &activeTranscriptSegments{index: n - 1, start: size, header: e.seg.Header, stable: e.seg.Stable, tail: e.seg.Tail}
			}
			m.activeSegSource = e.seg
		}
	}
	rows := m.publishRowCapacity(size, full || !m.geomEpoch)
	// Each materialized entry is placed at its slot offset, found in O(log N).
	// Offsets of materialized slots are patched; other lineOffsets stay as the
	// deterministic stale facade until ensureAllItemsRendered writes them exactly.
	place := func(i int, e renderedItemRange) renderedItemRange {
		m.stats.facadeEntries++
		if i >= n {
			return e
		}
		off := m.geom.Offset(i)
		m.lineOffsets[i] = off
		if !e.virtual && len(e.lines) > 0 && off+len(e.lines) <= rows {
			copy(m.renderedLines[off:], e.lines)
			e.start, e.span = off, len(e.lines)
		}
		return e
	}
	for i, e := range m.renderedItems {
		m.renderedItems[i] = place(i, e)
	}
	for i, e := range m.uncached {
		m.uncached[i] = place(i, e)
	}
	// The bottom window (the slots a pinned reader lands on) keeps exact facade
	// offsets even when it is not materialized, so a reader scrolled away from
	// the end sees the same offsets as one whose window stayed materialized.
	windowRows := 0
	for i := n - 1; i >= m.historyStart && i >= 0 && windowRows < m.height; i-- {
		if !m.hiddenSlot(i) {
			m.lineOffsets[i] = m.geom.Offset(i)
		}
		windowRows += m.geom.Height(i)
	}
	m.geomEpoch = false
	m.activeSegments = active
	m.totalHeight = total
	m.urlSpans.clear()
	if full {
		for i := range m.views {
			m.lineOffsets[i] = m.slotOffset(i)
		}
	}
}

// publishRowCapacity returns the mirror length for a publish that targets size
// rows. Shrinking is free. Growing reuses the backing array or allocates with
// headroom, but only when grow is set: an epoch that is not an explicit full
// demand keeps the current length, so its allocation never scales with history.
// It maintains only the compatibility facade backing/length; never read for layout
// decisions; called only from publishFacade.
func (m *model) publishRowCapacity(size int, grow bool) int {
	cur := len(m.renderedLines)
	switch {
	case size <= cur:
		m.renderedLines = m.renderedLines[:size]
		return size
	case !grow:
		return cur
	case size <= cap(m.renderedLines):
		m.renderedLines = m.renderedLines[:size]
		return size
	}
	grown := make([]string, size, size+size/2+16)
	copy(grown, m.renderedLines)
	m.renderedLines = grown
	m.stats.rowsAllocated += uint64(cap(grown))
	return size
}

// layoutTotal is the authority transcript height: the sum of the slot index,
// read after the index has caught up with the views. Only the direct-write
// harness (legacyFacadeMode) answers from the facade.
func (m *model) layoutTotal() int {
	if m.legacyFacadeMode() {
		return m.legacyTotal()
	}
	m.syncSlots()
	return m.geom.Total()
}

// layoutActiveStart is the first row of the segmented live tail, from the index.
// It is -1 when there is no segmented tail.
func (m *model) layoutActiveStart() int {
	s := m.activeSegments
	if s == nil || s.index < 0 || s.index >= len(m.views) {
		return -1
	}
	return m.geom.Offset(s.index)
}

// legacyFacadeMode reports the direct-write harness: a model with neither views
// nor messages, whose facade fields are the geometry its caller wrote. It is the
// only production predicate under which the facade is read as authority.
func (m *model) legacyFacadeMode() bool {
	return len(m.views) == 0 && len(m.messages) == 0
}

// legacyTotal, legacyOffset and legacyRow are the only facade readers. They are
// reached solely through legacyFacadeMode, so the facade cannot decide anything
// for a model that has views.
func (m *model) legacyTotal() int { return m.totalHeight }

// legacyOffsetCount is the facade's offset count, the bound a direct-write
// harness gives its own message indexes.
func (m *model) legacyOffsetCount() int { return len(m.lineOffsets) }

func (m *model) legacyOffset(i int) int {
	if i >= 0 && i < len(m.lineOffsets) {
		return m.lineOffsets[i]
	}
	return 0
}

func (m *model) legacyRow(g int) string {
	if g >= 0 && g < len(m.renderedLines) {
		return m.renderedLines[g]
	}
	return ""
}

// resetFacade zeroes the facade. It is test-visible compatibility semantics only
// (an invalidated or restarted transcript reads as empty) and never decides.
func (m *model) resetFacade() {
	m.renderedLines = nil
	m.lineOffsets = nil
	m.resetFacadeTotal()
}

// resetFacadeTotal zeroes only the published total; the mirror keeps its length
// until the next layout, which is the test-visible invalidation contract.
func (m *model) resetFacadeTotal() { m.totalHeight = 0 }

// settleHoverAnchor cancels a reference hover when the hovered item's authority
// offset moved since the hover was recorded. The facade is not consulted, so a
// measurement elsewhere in the transcript cannot cancel a hover on this item.
func (m *model) settleHoverAnchor() {
	if m.referenceHoverSlot < 0 || (len(m.referenceHoverValues) == 0 && m.hoveredURL == nil) {
		return
	}
	// Re-resolve the hovered item from its identity: a moved or removed message
	// cancels, a message that stayed at its offset keeps the hover.
	slot := m.referenceHoverSlot
	if msg := m.referenceHoverTarget.message; msg != nil {
		slot = m.messagePointerIndex(msg)
	}
	if slot < 0 || slot >= len(m.views) || m.slotOffset(slot) != m.referenceHoverOffset {
		m.CancelReferenceHover()
		return
	}
	m.referenceHoverSlot = slot
}

// pruneRetained drops materialized bodies that no owner needs. The window is
// the viewport plus one viewport height on each side, so a small resize or a
// one-screen scroll finds its rows still materialized; the live tail and the rows a
// reply handoff reads are kept. It runs before the pass measures, so the bodies
// this pass measures stay until the next pass decides on them.
func (m *model) pruneRetained() {
	n := len(m.views)
	margin := max(1, m.height)
	lo := max(0, m.scrollOffset-margin)
	hi := m.scrollOffset + max(1, m.height) + margin
	first := m.geom.Find(lo)
	last := first
	for i := first; i < n; i++ {
		if m.slotOffset(i) >= hi {
			break
		}
		last = i
	}
	keep := m.handoffRetained()
	retained := func(i int) bool {
		if (i >= first && i <= last) || m.isLiveTail(i) {
			return true
		}
		_, ok := keep[i]
		return ok
	}
	for i := range m.renderedItems {
		if !retained(i) {
			m.dropEntry(i)
		}
	}
	for i := range m.uncached {
		if !retained(i) {
			m.dropEntry(i)
		}
	}
}

// isLiveTail reports whether slot i is the last slot and holds the message the live
// stream last wrote. The pin is set by non-empty chunks and released when the reply
// completes, is cancelled, or the outer stream stops, so a finished, replayed or
// superseded tail is not kept. It decides retention only; tailLive still drives the
// dirty-tail measure.
func (m *model) isLiveTail(i int) bool {
	return m.liveTailMsg != nil && i == len(m.views)-1 && i < len(m.messages) && m.messages[i] == m.liveTailMsg
}

// measureBottomWindow makes the transcript end exact: it measures placeholder
// slots backwards from the last one until their rows cover the viewport. A
// bottom-pinned reader is placed at the total, so measuring only the rows it
// lands on converges in one pass instead of chasing estimates downwards.
func (m *model) measureBottomWindow() {
	// Measuring can shrink the total, and updateScrollState then adds bottom slack,
	// which hides the rows at the bottom. The window is re-derived after every slot,
	// so only rows the reader will still show after the slack are measured.
	rows := 0
	for i := len(m.views) - 1; i >= m.historyStart && i >= 0; i-- {
		if rows >= m.height-m.predictedBottomSlack() {
			break
		}
		if !m.materialized(i) && !m.hiddenSlot(i) {
			m.measureSlot(i)
		}
		rows += m.geom.Height(i)
	}
}

// predictedBottomSlack is the slack updateScrollState would settle to for the
// current total. It is derived from the follow baseline, so after an epoch
// (baseline 0) it is 0 and the window is exactly the visible rows.
func (m *model) predictedBottomSlack() int {
	return m.settledBottomSlack(m.followBaseline(), m.layoutTotal(), true)
}

// markTailChanged flags the live tail for one re-measure when its own content
// changes. Epochs never call it, so a width or theme change leaves the tail stale.
func (m *model) markTailChanged(index int) {
	if index == len(m.views)-1 {
		m.tailDirty = true
	}
}

// tailLive reports whether slot i is the live streaming tail: a deferred chunk or
// an active segmented suffix. activeSegments is the stream's segmented-suffix
// metadata, built when the live item's content changes, so a changed live tail is
// measured even while the viewer is scrolled away from it. A finished or idle tail
// is not.
func (m *model) tailLive(i int) bool {
	if i < 0 || i >= len(m.views) {
		return false
	}
	if len(m.deferredTail) != 0 {
		return true
	}
	return m.activeSegments != nil && m.activeSegments.index == i
}

// layoutViewport materializes the slots that intersect the viewport, plus the
// anchored top row after a width epoch. It never walks history: the stale
// estimates of every other slot stay in the index untouched.
func (m *model) layoutViewport() {
	m.syncWorkProjection()
	m.workCommands = nil
	if m.replay != nil {
		return
	}
	// A dirty layout changes rows the frame key does not describe.
	if m.renderDirty {
		m.invalidateFrameContent()
	}
	if generation := styles.ThemeGeneration(); m.themeGeneration != generation {
		m.captureViewportAnchor()
		m.themeGeneration = generation
		if m.inlineEditMsgIndex >= 0 {
			m.inlineEditTextarea.SetStyles(inlineEditStyles())
		}
		m.invalidateAllItems()
	}
	m.syncSlots()
	if m.legacyFacadeMode() {
		// Direct-write harness: the facade is the geometry the caller wrote, and
		// only an explicit rebuild request clears it.
		if m.renderDirty {
			m.dropMaterialized()
			m.resetFacade()
			m.renderDirty = false
		}
		return
	}
	n := len(m.views)
	m.pruneRetained()
	m.measurePending()
	m.restoreViewportAnchor()
	top := m.scrollOffset
	bottom := top + max(1, m.height)
	if !m.userHasScrolled {
		// The first layout of a loaded transcript places a pinned reader at the end
		// (updateScrollState), so the offset-0 rows it does not see are not measured.
		m.measureBottomWindow()
		if m.needsPlacement {
			bottom = 0
		}
	}
	for i := m.geom.Find(top); i < n; i++ {
		if m.slotOffset(i) >= bottom {
			break
		}
		if !m.materialized(i) && !m.hiddenSlot(i) && i >= m.historyStart {
			m.measureSlot(i)
		}
	}
	// A live tail whose content changed is measured even when it is offscreen (see
	// tailLive). An epoch only leaves it stale.
	if tail := n - 1; m.tailDirty && m.tailLive(tail) && !m.materialized(tail) && !m.hiddenSlot(tail) {
		m.measureSlot(tail)
	}
	m.tailDirty = false
	m.needsPlacement = false
	// Measuring can move the window: a slot materialized from a stale estimate
	// may now lie outside it. Publishing after the pass keeps the facade a
	// function of the settled authority, not of the estimates before this pass.
	m.settleHoverAnchor()
	m.publishFacade(false)
	m.renderDirty = false
}

// workRangeList materializes the range metadata of every work block: anchor,
// block, first row and measured height. It walks every block, so no hot path
// calls it. Layout, mouse, wheel, hover, copy and handoff resolve one block
// through workBlockAt, and only explicit diagnostics or tests read this list.
// It never renders a block body.
func (m *model) workRangeList() []workBlockRange {
	m.syncSlots()
	m.workRanges = m.workRanges[:0]
	for anchor, block := range m.workHeaderBySource {
		m.stats.workRangeScans++
		if block == nil || anchor < 0 || anchor >= len(m.views) {
			continue
		}
		m.workRanges = append(m.workRanges, workBlockRange{block: block, anchor: anchor, start: m.geom.Offset(anchor), height: m.slotBase[anchor]})
	}
	slices.SortFunc(m.workRanges, func(a, b workBlockRange) int { return a.anchor - b.anchor })
	return m.workRanges
}

// renderedLineAt returns the flattened row at global line g, materializing the
// owning slot when it is not yet materialized.
func (m *model) renderedLineAt(g int) string {
	if g < 0 {
		return ""
	}
	if m.legacyFacadeMode() {
		return m.legacyRow(g)
	}
	if start := m.layoutActiveStart(); start >= 0 && g >= start && g < start+m.activeSegments.height() {
		return m.activeSegments.line(g - start)
	}
	slot := m.slotAt(g)
	if slot >= len(m.views) {
		return ""
	}
	if !m.materialized(slot) {
		m.measureSlot(slot)
	}
	m.publishFacade(false)
	e, _ := m.entryFor(slot)
	local := g - m.geom.Offset(slot)
	if e.virtual {
		if s := m.activeSegments; s != nil && local >= 0 {
			return s.line(local)
		}
		return ""
	}
	if local >= 0 && local < len(e.lines) {
		return e.lines[local]
	}
	return ""
}

// splitRendered splits rendered text into lines without the trailing newline.
func splitRendered(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
