package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type majorEventFence struct{ epoch, sequence uint64 }

// Only canonical journal facts count. Seed deliveries advance the high-water
// fence but never create counts; raw stream/tree notifications are not facts.
func (m *appModel) observeMajorEvent(msg messages.SessionRuntimeEventMsg) tea.Cmd {
	owner := msg.OriginSessionID
	if owner == "" || msg.Sequence == 0 || msg.Event == nil {
		return nil
	}
	if m.majorEventWater == nil {
		m.majorEventWater = make(map[string]majorEventFence)
	}
	previous := m.majorEventWater[owner]
	if msg.Epoch < previous.epoch || msg.Sequence <= previous.sequence {
		return nil
	}
	// Retain only journals still represented by a live supervisor owner. This
	// fence is observer state, not closed-session notification history.
	for recorded := range m.majorEventWater {
		if recorded != owner && m.supervisor.FindBySession(recorded) == nil {
			delete(m.majorEventWater, recorded)
		}
	}
	if m.supervisor.FindBySession(owner) == nil {
		return nil
	}
	m.majorEventWater[owner] = majorEventFence{epoch: msg.Epoch, sequence: msg.Sequence}
	if msg.Seed {
		return nil
	}
	if fact, ok := msg.Event.(*runtime.TurnSettledEvent); ok {
		if fact.SessionID != owner || fact.TurnID == "" || msg.TurnID != fact.TurnID {
			return nil
		}
		if m.paneOutcomes == nil {
			m.paneOutcomes = make(map[string]runtime.TurnOutcome)
		}
		m.paneOutcomes[owner] = fact.Outcome
		m.viewCacheValid = false
	}
	return nil
}
