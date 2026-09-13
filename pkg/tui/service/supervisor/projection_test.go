package supervisor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSharedProjectionSeedsRunningTitleAndInteraction(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	prompt := runtime.ElicitationRequest("input", "form", nil, "", "e", "", "session", nil, "agent").(*runtime.ElicitationRequestEvent)
	prompt.RequestID = "request"
	head := &app.PresentationState{Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateRunning}, Interactions: []runtime.InteractionSnapshot{{SessionID: "session", InteractionID: "request", Event: prompt}}}
	s.applyPresentation("tab", head, &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: session.New(session.WithTitle("canonical"))}})
	tabs, _ := s.GetTabs()
	assert.Equal(t, "canonical", tabs[0].Title)
	assert.Equal(t, messages.TabActivityRunning, tabs[0].Activity)
	assert.True(t, tabs[0].NeedsAttention)
	assert.Same(t, head, s.GetRunner("tab").projection)
	assert.Same(t, prompt, s.ConsumePendingEvent("tab"))
}

func TestSharedProjectionReplacementRemovesOnlyStalePending(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	first := &runtime.MaxIterationsReachedEvent{SessionID: "session", RequestID: "first"}
	live := &runtime.MaxIterationsReachedEvent{SessionID: "session", RequestID: "live"}
	head := &app.PresentationState{Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateRunning}, Interactions: []runtime.InteractionSnapshot{{Event: first}, {Event: live}}}
	s.applyPresentation("tab", head, nil)
	replacement := &app.PresentationState{Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateSettled}, Interactions: []runtime.InteractionSnapshot{{Event: live}}}
	s.applyPresentation("tab", replacement, &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: session.New(session.WithTitle("recovered"))}})
	tabs, _ := s.GetTabs()
	assert.Equal(t, "recovered", tabs[0].Title)
	assert.Equal(t, messages.TabActivityNone, tabs[0].Activity)
	assert.Equal(t, []tea.Msg{live}, s.GetRunner("tab").PendingEvents)
	s.SetPendingEvent("tab", first)
	assert.Equal(t, []tea.Msg{live}, s.GetRunner("tab").PendingEvents)
	s.applyPresentation("tab", &app.PresentationState{Status: replacement.Status}, &runtime.InteractionResolvedEvent{SessionID: "session", InteractionID: "live"})
	assert.Empty(t, s.GetRunner("tab").PendingEvents)
	tabs, _ = s.GetTabs()
	assert.False(t, tabs[0].NeedsAttention)
}

func TestSharedProjectionRuntimeErrorNeedsInactiveTabAttention(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	s.applyPresentation("tab", nil, runtime.Error("transport failed"))
	tabs, _ := s.GetTabs()
	assert.True(t, tabs[0].NeedsAttention)
	s.CloseSession("tab")
	s.applyPresentation("tab", &app.PresentationState{}, nil)
	assert.Empty(t, s.order)
}
