package messages

import (
	"time"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type sentInputScroll struct {
	enabled    bool
	pending    []string
	seen       map[string]struct{}
	target     *types.Message
	padding    int
	reserve    *types.Message
	transition animation.Transition
	from       int
}

// SetScrollOnSend enables only explicit accepted-send requests. Ingestion and
// replay never infer a local send, and changing the setting never navigates.
func (m *model) SetScrollOnSend(enabled bool) {
	m.scrollOnSend.enabled = enabled
	if !enabled {
		m.cancelSentInputScroll()
		m.scrollOnSend.reserve = nil
	}
}

// ScrollToSentInput may precede or follow transcript ingestion. Callers pass
// only new immediate local turns, never queued/steer receipts. Duplicate receipts
// do not reassert navigation after the reader has taken control.
func (m *model) ScrollToSentInput(turnID string) {
	s := &m.scrollOnSend
	if !s.enabled || turnID == "" || m.replay != nil {
		return
	}
	if _, duplicate := s.seen[turnID]; duplicate {
		return
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	s.seen[turnID] = struct{}{}
	s.pending = []string{turnID}
	s.transition.Cancel()
	s.target = nil
	m.invalidateView()
	m.lastFrameOutput = ""
	m.updateScrollState()
}

func (m *model) cancelSentInputScroll() {
	s := &m.scrollOnSend
	if s.target == nil && len(s.pending) == 0 && s.padding == 0 {
		return
	}
	s.pending, s.target = nil, nil
	s.transition.Cancel()
	m.lastFrameOutput = ""
	m.invalidateView()
}

func (m *model) prepareSentInputScroll() {
	s := &m.scrollOnSend
	if m.replay != nil || !s.enabled {
		m.cancelSentInputScroll()
		s.reserve, s.padding = nil, 0
		return
	}
	// Match only the latest explicit receipt. The caller filters queued sends;
	// pending/non-user cards cannot accidentally become a navigation target.
	for p := len(s.pending) - 1; p >= 0; p-- {
		for i := len(m.messages) - 1; i >= m.historyStart; i-- {
			msg := m.messages[i]
			if msg.TurnID != s.pending[p] || msg.Type != types.MessageTypeUser || msg.Pending {
				continue
			}
			s.target = msg
			s.reserve = msg
			s.from = m.scrollOffset
			s.transition.Start(420*time.Millisecond, animation.EaseInOutCubic)
			s.pending = append(s.pending[:p], s.pending[p+1:]...)
			m.userHasScrolled = true
			m.bottomSlack = 0
			m.syncWorkProjection()
			m.syncSlots()
			m.measureSlot(i)
			return
		}
	}
}

// placeSentInputScroll keeps reply room source-owned even after the reader
// interrupts navigation. Real response rows, not scrolling, consume the room.
func (m *model) placeSentInputScroll() {
	s := &m.scrollOnSend
	if s.reserve == nil {
		s.padding = 0
		return
	}
	if m.height < 6 {
		s.target, s.reserve, s.padding = nil, nil, 0
		s.transition.Cancel()
		m.userHasScrolled = false
		m.scrollOffset = max(0, m.displayTotalHeight()-m.height)
		m.scrollview.SetScrollOffset(m.scrollOffset)
		return
	}
	index := m.messagePointerIndex(s.reserve)
	if index < m.historyStart || index >= len(m.views) {
		m.cancelSentInputScroll()
		s.reserve, s.padding = nil, 0
		return
	}
	if !m.materialized(index) {
		m.measureSlot(index)
	}
	// Preserve the preceding separator for short prompts; long prompts keep
	// their ending visible and reserve at least a third of the viewport.
	promptStart := m.slotOffset(index)
	promptEnd := promptStart + m.slotBase[index]
	gap := 0
	if index > m.historyStart && m.slotSep[m.separatorOwnerFrom(index-1)] {
		gap = 1
	}
	replyRows := max(3, m.height/3)
	top := max(0, max(promptStart-gap, promptEnd+replyRows-m.height))
	total := m.displayTotalHeight()
	s.padding = max(0, top+m.height-total)
	if s.target == nil {
		if s.padding == 0 {
			s.reserve = nil
		}
		return
	}
	offset := top
	if s.transition.Running() {
		offset = s.transition.Lerp(s.from, top)
	}
	m.scrollOffset = offset
	m.scrollview.SetScrollOffset(offset)
	if s.padding == 0 {
		s.transition.Cancel()
		s.reserve = nil
		s.target = nil
		m.userHasScrolled = false
		m.scrollOffset = max(0, total-m.height)
		m.scrollview.SetScrollOffset(m.scrollOffset)
	}
}

// Only the accepted prompt bypasses edge fading; history and reply rows retain it.
func (m *model) sentInputFadeRows() (int, int) {
	source := m.scrollOnSend.reserve
	if source == nil {
		return 0, 0
	}
	index := m.messagePointerIndex(source)
	if index < m.historyStart || index >= len(m.views) {
		return 0, 0
	}
	start := m.slotOffset(index) - m.scrollOffset
	if _, _, sticky := m.stickyWork(); sticky {
		start++
	}
	return start, start + m.slotBase[index]
}
