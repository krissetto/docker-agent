package messages

import (
	"math"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type hoverActionValue struct{ value, target float64 }

func (m *model) hoverActionProgress(index int) float64 {
	if index < 0 || index >= len(m.messages) {
		return 0
	}
	if m.focused && index == m.selectedMessageIndex {
		return 1
	}
	return m.hoverActionValues[m.messages[index]].value
}

func (m *model) retargetHoverActions(index int) {
	if m.hoverActionValues == nil {
		m.hoverActionValues = make(map[*types.Message]hoverActionValue)
	}
	var target *types.Message
	if index >= 0 && index < len(m.messages) {
		target = m.messages[index]
	}
	for key, state := range m.hoverActionValues {
		if !m.containsMessagePointer(key) {
			delete(m.hoverActionValues, key)
			continue
		}
		state.target = 0
		if key == target {
			state.target = 1
		}
		if state.value == 0 && state.target == 0 {
			delete(m.hoverActionValues, key)
		} else {
			m.hoverActionValues[key] = state
		}
	}
	if target != nil {
		state, exists := m.hoverActionValues[target]
		if !exists {
			state.value = 1.0 / 3
		}
		state.target = 1
		m.hoverActionValues[target] = state
	}
	if m.hoverActionMoving() {
		m.hoverActionAnimation.Start()
	} else {
		m.hoverActionAnimation.Stop()
	}
}

func (m *model) containsMessagePointer(target *types.Message) bool {
	return m.messagePointerIndex(target) >= 0
}

func (m *model) messagePointerIndex(target *types.Message) int {
	for i, msg := range m.messages {
		if msg == target {
			return i
		}
	}
	return -1
}

func (m *model) hoverActionMoving() bool {
	for _, state := range m.hoverActionValues {
		if state.value != state.target {
			return true
		}
	}
	return false
}

func (m *model) tickHoverActions(tick animation.TickMsg) {
	if !m.hoverActionAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	changed := make([]int, 0, len(m.hoverActionValues))
	for key, state := range m.hoverActionValues {
		old := state.value
		state.value = animation.HoverStep(state.value, state.target, after-before)
		index := m.messagePointerIndex(key)
		if index < 0 {
			delete(m.hoverActionValues, key)
			continue
		}
		if math.Abs(state.value-old) > 1e-12 {
			changed = append(changed, index)
		}
		if state.value == 0 && state.target == 0 {
			delete(m.hoverActionValues, key)
		} else {
			m.hoverActionValues[key] = state
		}
	}
	if !m.hoverActionMoving() {
		m.hoverActionAnimation.Stop()
	}
	if len(changed) == 0 {
		return
	}
	m.refreshHoverItems(changed...)
	m.hoverActionGeneration++
	m.visualGeneration++
	tick.MarkDirty()
}

// CancelPointerHover immediately cancels all pointer-owned animation and targets.
func (m *model) CancelPointerHover() {
	m.pointerKnown = false
	m.hoveredMessageIndex = -1
	m.hoverActionAnimation.Stop()
	indices := make([]int, 0, len(m.hoverActionValues))
	for message := range m.hoverActionValues {
		if index := m.messagePointerIndex(message); index >= 0 {
			indices = append(indices, index)
		}
	}
	m.hoverActionValues = nil
	m.clearToolHover()
	m.CancelReferenceHover()
	m.cancelImageClick()
	m.attachmentClick = nil
	m.refreshHoverItems(indices...)
	m.hoverActionGeneration++
	m.visualGeneration++
}

// ClearPointerHover is the external occlusion path (dialogs, presentation hides): it
// also forgets the tracked cell so a later fade cannot re-hover underneath it.
func (m *model) ClearPointerHover() tea.Cmd {
	m.pointerKnown = false
	return m.clearPointerHoverTargets()
}

// clearPointerHoverTargets drops hover targets but keeps the tracked pointer cell,
// for handoff geometry changes that must re-resolve hover under a still pointer.
func (m *model) clearPointerHoverTargets() tea.Cmd {
	old := m.hoveredMessageIndex
	changed := old >= 0 || m.hoveredToolIndex >= 0 || m.hoveredURL != nil || m.attachmentClick != nil || m.imageClick != nil
	if old >= 0 {
		m.hoveredMessageIndex = -1
		m.retargetHoverActions(-1)
		m.refreshHoverItems(old)
		m.hoverActionGeneration++
	} else if m.hoverActionMoving() {
		m.retargetHoverActions(-1)
	}
	m.clearToolHover()
	m.ClearReferenceHover()
	m.cancelImageClick()
	m.attachmentClick = nil
	if changed {
		m.visualGeneration++
	}
	return nil
}
