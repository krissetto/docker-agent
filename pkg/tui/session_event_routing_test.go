package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestWrappedActiveRuntimeEventsKeepModelSideEffects(t *testing.T) {
	t.Parallel()
	m, _ := newCompactTestModel(t, stubRuntime{}, session.New())
	m.sessionState = service.NewSessionState(session.New())

	team := &runtime.TeamInfoEvent{
		AvailableAgents: []runtime.AgentDetails{{Name: "root"}, {Name: "helper"}},
		CurrentAgent:    "helper",
	}
	_, _ = m.Update(messages.SessionRuntimeEventMsg{Event: team, Seed: true})
	assert.Equal(t, team.AvailableAgents, m.sessionState.AvailableAgents())
	assert.Equal(t, "helper", m.sessionState.CurrentAgentName())

	agent := &runtime.AgentInfoEvent{AgentName: "root", Model: "test/model"}
	_, _ = m.Update(messages.SessionRuntimeEventMsg{Event: agent})
	assert.Equal(t, "root", m.sessionState.CurrentAgentName())
	assert.Equal(t, "test/model", m.application.CurrentAgentModel(t.Context()))

	title := &runtime.SessionTitleEvent{Title: "wrapped title"}
	_, _ = m.Update(messages.SessionRuntimeEventMsg{Event: title})
	assert.Equal(t, "wrapped title", m.sessionState.SessionTitle())
}
