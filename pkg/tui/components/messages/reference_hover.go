package messages

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// A message is an occurrence: equal short IDs, and repeated references to the
// same canonical agent, must never share pointer presentation.
type referenceHoverKey struct {
	message *types.Message
	kind    lifecycle.InputReferenceKind
	id      string
}

type referenceHoverValue struct{ value, target float64 }

func (m *model) referenceKey(index int, ref lifecycle.InputReference) referenceHoverKey {
	return referenceHoverKey{message: m.messages[index], kind: ref.Kind, id: ref.ID}
}

func (m *model) setReferenceHover(key referenceHoverKey) {
	if m.referencePresentationHidden || key == m.referenceHoverTarget {
		return
	}
	m.referenceHoverTarget = key
	if m.referenceHoverValues == nil {
		m.referenceHoverValues = make(map[referenceHoverKey]referenceHoverValue)
	}
	if key.message != nil {
		if _, ok := m.referenceHoverValues[key]; !ok {
			m.referenceHoverValues[key] = referenceHoverValue{}
		}
	}
	running := false
	for k, state := range m.referenceHoverValues {
		state.target = 0
		if k == key {
			state.target = 1
		}
		if state.value == 0 && state.target == 0 {
			delete(m.referenceHoverValues, k)
		} else {
			m.referenceHoverValues[k] = state
		}
		running = running || state.value != state.target
	}
	if running {
		m.referenceHoverAnimation.Start()
	} else {
		m.referenceHoverAnimation.Stop()
	}
}

func (m *model) tickReferenceHover(tick animation.TickMsg) {
	if !m.referenceHoverAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	changed, running := false, false
	for key, state := range m.referenceHoverValues {
		old := state.value
		state.value = animation.HoverStep(state.value, state.target, after-before)
		changed = changed || old != state.value
		running = running || state.value != state.target
		if state.value == 0 && state.target == 0 {
			delete(m.referenceHoverValues, key)
		} else {
			m.referenceHoverValues[key] = state
		}
	}
	if !running {
		m.referenceHoverAnimation.Stop()
	}
	if changed {
		m.referenceHoverGeneration++
		m.invalidateView()
		tick.MarkDirty()
	}
}

func (m *model) applyReferenceHover(lines []string, start int) {
	if len(m.referenceHoverValues) == 0 {
		return
	}
	for i, line := range lines {
		global := start + i
		reference := m.renderedReference(global)
		if reference.key.message == nil {
			continue
		}
		state := m.referenceHoverValues[reference.key]
		if state.value <= 0 {
			continue
		}
		for _, span := range m.urlSpans.get(global, m.renderedLine(global)) {
			if span.url == agentidentity.Link {
				line = agentidentity.HoverProgress(line, span.startCol, span.endCol, reference.ref, state.value)
			}
		}
		lines[i] = line
	}
}

// ClearReferenceHover fades pointer presentation without touching content caches.
func (m *model) ClearReferenceHover() tea.Cmd {
	m.setReferenceHover(referenceHoverKey{})
	if m.hoveredURL != nil {
		m.hoveredURL = nil
		m.invalidateView()
	}
	return nil
}

func (m *model) CancelReferenceHover() {
	m.referenceHoverAnimation.Stop()
	if len(m.referenceHoverValues) > 0 || m.hoveredURL != nil {
		m.referenceHoverGeneration++
		m.invalidateView()
	}
	m.referenceHoverValues = nil
	m.referenceHoverTarget = referenceHoverKey{}
	m.hoveredURL = nil
}

func (m *model) SetReferencePresentationActive(active bool) {
	m.referencePresentationHidden = !active
	if !active {
		m.CancelReferenceHover()
	}
}

func (m *model) pruneReferenceHover() {
	if len(m.referenceHoverValues) == 0 {
		return
	}
	valid := make(map[referenceHoverKey]bool)
	for i := range m.messages {
		if ref, ok := m.referenceForMessage(i, 0); ok {
			valid[m.referenceKey(i, ref)] = true
		}
	}
	for key := range m.referenceHoverValues {
		if !valid[key] {
			m.CancelReferenceHover()
			return
		}
	}
}

type renderedReference struct {
	key referenceHoverKey
	ref lifecycle.InputReference
}

// Reference geometry shares the rendered-line cache lifetime, not the tick lifetime.
func (m *model) renderedReference(global int) renderedReference {
	if cached, ok := m.urlSpans.references[global]; ok {
		return cached
	}
	var result renderedReference
	index, local := m.globalLineToMessageLineCached(global)
	if ref, ok := m.referenceForMessage(index, local); ok {
		result = renderedReference{key: m.referenceKey(index, ref), ref: ref}
	}
	m.urlSpans.references[global] = result
	return result
}
