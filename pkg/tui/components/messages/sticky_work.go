package messages

import (
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type stickyWorkLatch struct {
	active  bool
	anchor  int
	block   *workblock.Model
	visible bool
	offset  int
}

// stickyWork derives the overlay from the exchange at the viewport's top.
// It reserves one viewport row; scrollOffset remains the first body row.
func (m *model) stickyWork() (int, *workblock.Model, bool) {
	if !m.stickyWorkEnabled() {
		return 0, nil, false
	}
	active := m.replyHandoff != nil && m.replyHandoff.Active()
	if !active {
		m.stickyWorkLatch = stickyWorkLatch{}
	} else if m.stickyWorkLatch.active && m.stickyWorkLatch.offset == m.scrollOffset {
		latch := m.stickyWorkLatch
		return latch.anchor, latch.block, latch.visible && m.workHeaderBySource[latch.anchor] == latch.block
	}
	anchor, block, visible := m.sectionStickyWork(active)
	if active {
		m.stickyWorkLatch = stickyWorkLatch{active: true, anchor: anchor, block: block, visible: visible, offset: m.scrollOffset}
	}
	return anchor, block, visible
}

func (m *model) sectionStickyWork(animated bool) (int, *workblock.Model, bool) {
	return m.sectionStickyWorkAt(m.scrollOffset, animated)
}

func (m *model) sectionStickyWorkAt(offset int, animated bool) (int, *workblock.Model, bool) {
	source := m.slotAt(offset)
	if animated {
		if row, ok := m.replyHandoffRowAt(offset); ok {
			if row.Composite != nil {
				source = row.CompositeAnchor
			} else if row.Source != nil {
				source = row.SourceIndex
			}
		}
	}
	exchangeIndex, ok := m.workProjection.owner[source]
	if !ok || exchangeIndex < 0 || exchangeIndex >= len(m.workProjection.exchanges) {
		return 0, nil, false
	}
	exchange := m.workProjection.exchanges[exchangeIndex]
	if exchange.boundary < 0 || exchange.boundary >= len(m.messages) || m.messages[exchange.boundary].Type != types.MessageTypeUser || len(exchange.work) == 0 {
		return 0, nil, false
	}
	// Paging may retain a prefix of this exchange outside the current layout.
	anchor := -1
	for _, work := range exchange.work {
		if work >= m.historyStart {
			anchor = work
			break
		}
	}
	block := m.workHeaderBySource[anchor]
	if block == nil || anchor >= len(m.slotKnown) {
		return 0, nil, false
	}
	// Only the section's last visible slot needs an exact height. A response
	// reached from below is already measured, but its offscreen Work anchor may
	// still be an estimate (recent-first replay/paging). Requiring that anchor
	// to be known makes the overlay appear only after scrolling past it first.
	last := anchor
	for _, indices := range [][]int{exchange.work, exchange.response} {
		if len(indices) == 0 {
			continue
		}
		source := indices[len(indices)-1]
		if owner, grouped := m.workSourceToHeader[source]; grouped {
			source = owner
		}
		if m.geom.position(source) > m.geom.position(last) {
			last = source
		}
	}
	if last >= len(m.slotKnown) || !m.slotKnown[last] {
		return 0, nil, false
	}
	end := m.slotOffset(last) + m.slotBase[last]
	headerEnd := m.slotOffset(anchor) + 1
	// A live collapsed preview needs its header even before a long response
	// exists. Four-row viewports also need room to reach the section end.
	preview := !block.Expanded() && m.slotKnown[anchor] && m.slotBase[anchor] >= 4
	if (!preview && end-headerEnd < 6) || end-offset < min(3, m.height-2) {
		return 0, nil, false
	}
	if m.replyHandoff != nil && m.replyHandoff.Active() {
		// Use painted rather than destination geometry during the handoff. If
		// duplicate header rows overlap, all must have left the viewport first.
		found := false
		for _, row := range m.replyHandoffFrameRows {
			if row.Composite != block || row.CompositeLine != 0 || row.Height <= 0 {
				continue
			}
			_, end := replyHandoffTerminalSpan(row.Y, row.Height)
			if !found || end > headerEnd {
				headerEnd = end
			}
			found = true
		}
	}
	if headerEnd > offset {
		return 0, nil, false
	}
	return anchor, block, true
}

func (m *model) stickyWorkAt(x, y int) (int, *workblock.Model, bool) {
	if y != m.yPos || x < m.xPos || x >= m.xPos+m.contentWidth() {
		return 0, nil, false
	}
	return m.stickyWork()
}

func (m *model) activateStickyWork(block *workblock.Model) {
	anchor := m.workSourceForBlock(block)
	_, inWork := m.workSourceToHeader[m.slotAt(m.scrollOffset)]
	if !block.Expanded() || inWork {
		m.cancelSentInputScroll()
		// An explicit reveal is navigation; folding while reading the reply is
		// not, and must preserve the existing follow/pinned state.
		m.userHasScrolled = true
		m.setScrollOffset(m.slotOffset(anchor))
	} else {
		// Settle response-side folding so no intermediate disclosure frame can
		// displace the reader's source/local anchor.
		source, local := m.globalLineToMessageLineCanonical(m.scrollOffset)
		pinned := !m.userHasScrolled
		block.SetExpandedImmediately(false)
		m.invalidateItem(anchor)
		m.layoutViewport()
		if !pinned && source >= 0 {
			m.setScrollOffset(m.sourceToGlobalLine(source, local))
		}
		return
	}
	m.toggleWork(block)
}

// Selection cannot start on the header. A drag reaching its top edge
// stops at the first body cell;
// edge autoscroll can still reveal earlier transcript rows normally.
func (m *model) selectionMouseToLineCol(x, y int) (int, int) {
	if y <= m.yPos {
		if _, _, ok := m.stickyWork(); ok {
			return m.scrollOffset, 0
		}
	}
	return m.mouseToLineCol(x, y)
}

func (m *model) stickyWorkEnabled() bool {
	return m.workEnabled && m.height >= 4 && m.contentWidth() >= 16 && m.replay == nil && !m.selection.hasRange() && len(m.workProjection.exchanges) != 0
}

func (m *model) stickyWorkInset() int {
	if _, _, ok := m.stickyWork(); ok {
		return 1
	}
	return 0
}

func (m *model) bodyHeight() int { return max(0, m.height-m.stickyWorkInset()) }

// Decide from the natural bottom, never from a total that already includes
// the reserve. This keeps the clamp stable as the sticky condition changes.
// The lookup uses only indexed geometry; it never measures an offscreen slot.
func (m *model) stickyWorkBottomReserve(total int) int {
	if !m.stickyWorkEnabled() || total <= m.height {
		return 0
	}
	if _, _, ok := m.sectionStickyWorkAt(total-m.height, m.replyHandoff != nil && m.replyHandoff.Active()); ok {
		return 1
	}
	return 0
}
