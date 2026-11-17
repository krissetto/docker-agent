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
// It owns no height: each section's natural header takes over
// as the reader crosses a user boundary, without changing scroll geometry.
func (m *model) stickyWork() (int, *workblock.Model, bool) {
	if !m.workEnabled || m.height < 6 || m.contentWidth() < 16 || m.replay != nil || m.selection.hasRange() || len(m.workProjection.exchanges) == 0 {
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
	source := m.slotAt(m.scrollOffset)
	if animated {
		if row, ok := m.replyHandoffRowAt(m.scrollOffset); ok {
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
	if block == nil || anchor >= len(m.slotKnown) || !m.slotKnown[anchor] {
		return 0, nil, false
	}
	end := 0
	for _, indices := range [][]int{exchange.work, exchange.response} {
		if len(indices) == 0 {
			continue
		}
		last := indices[len(indices)-1]
		if owner, grouped := m.workSourceToHeader[last]; grouped {
			last = owner
		}
		if last >= len(m.slotKnown) || !m.slotKnown[last] {
			return 0, nil, false
		}
		end = max(end, m.slotOffset(last)+m.slotBase[last])
	}
	headerEnd := m.slotOffset(anchor) + 1
	if end-headerEnd < 6 || end-m.scrollOffset < 3 {
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
	if headerEnd > m.scrollOffset {
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

// Selection cannot start on the overlay or extend into the obscured source
// row. A drag reaching its top edge stops at the first unobscured content cell;
// edge autoscroll can still reveal earlier transcript rows normally.
func (m *model) selectionMouseToLineCol(x, y int) (int, int) {
	if y <= m.yPos {
		if _, _, ok := m.stickyWork(); ok {
			return m.scrollOffset + 1, 0
		}
	}
	return m.mouseToLineCol(x, y)
}
