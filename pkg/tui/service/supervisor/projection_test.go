package supervisor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	s.applyPresentation("tab", nil, 0, head, &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: session.New(session.WithTitle("canonical"))}})
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
	s.applyPresentation("tab", nil, 0, head, nil)
	replacement := &app.PresentationState{Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateSettled}, Interactions: []runtime.InteractionSnapshot{{Event: live}}}
	s.applyPresentation("tab", nil, 0, replacement, &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: session.New(session.WithTitle("recovered"))}})
	tabs, _ := s.GetTabs()
	assert.Equal(t, "recovered", tabs[0].Title)
	assert.Equal(t, messages.TabActivityNone, tabs[0].Activity)
	assert.Equal(t, []tea.Msg{live}, s.GetRunner("tab").PendingEvents)
	s.SetPendingEvent("tab", first)
	assert.Equal(t, []tea.Msg{live}, s.GetRunner("tab").PendingEvents)
	s.applyPresentation("tab", nil, 0, &app.PresentationState{Status: replacement.Status}, &runtime.InteractionResolvedEvent{SessionID: "session", InteractionID: "live"})
	assert.Empty(t, s.GetRunner("tab").PendingEvents)
	tabs, _ = s.GetTabs()
	assert.False(t, tabs[0].NeedsAttention)
}

func TestSharedProjectionRuntimeErrorNeedsInactiveTabAttention(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	s.applyPresentation("tab", nil, 0, nil, runtime.Error("transport failed"))
	tabs, _ := s.GetTabs()
	assert.True(t, tabs[0].NeedsAttention)
	s.CloseSession("tab")
	s.applyPresentation("tab", nil, 0, &app.PresentationState{}, nil)
	assert.Empty(t, s.order)
}

// The delayed callback models a subscriber already entered before cancellation.
// It must not seed a replacement even when the persisted route key is reused.
func TestOldSubscriberCannotProjectAfterReplacement(t *testing.T) {
	for _, operation := range []string{"replace", "reopen", "retarget"} {
		t.Run(operation, func(t *testing.T) {
			s := New(nil)
			t.Cleanup(s.Shutdown)
			sess := session.New(session.WithID("tab"))
			_, err := s.AddSession(t.Context(), nil, sess, "", nil)
			require.NoError(t, err)
			generation, _ := s.RouteGeneration("tab")
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
			go func() {
				close(entered)
				<-release
				_, accepted := s.projectRoutedEvent("tab", nil, generation, app.SessionEventMsg{
					Projection: &app.PresentationState{Status: runtime.SessionStatus{State: runtime.SessionStateRunning}},
					Event:      &runtime.SessionTitleEvent{Title: "stale"},
				})
				done <- accepted
			}()
			<-entered
			id := "tab"
			switch operation {
			case "replace":
				s.ReplaceRunnerApp(t.Context(), id, SpawnedSession{Session: sess}, "replacement")
			case "reopen":
				s.CloseSession(id)
				_, err = s.AddSession(t.Context(), nil, sess, "replacement", nil)
				require.NoError(t, err)
			case "retarget":
				require.True(t, s.RetargetRunner(t.Context(), id, "retargeted", "replacement"))
				id = "retargeted"
			}
			current, exists := s.RouteGeneration(id)
			require.True(t, exists)
			require.Greater(t, current, generation)
			close(release)
			require.False(t, <-done)
			// Both direct refresh and raw tree invalidation use the same fence.
			s.applyPresentation(id, nil, generation, &app.PresentationState{}, &runtime.SessionTitleEvent{Title: "stale"})
			_, accepted := s.projectRoutedEvent(id, nil, generation, &runtime.SubagentTreeEvent{})
			require.False(t, accepted)
			tabs, _ := s.GetTabs()
			require.NotEqual(t, "stale", tabs[0].Title)
			require.False(t, tabs[0].IsRunning)
			require.Nil(t, s.GetRunner(id).projection)
		})
	}
}

func TestProjectionRequiresExpectedAppAsWellAsRoute(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "tab")
	a := app.New(t.Context(), nil, session.New(), runtime.SessionBinding{})
	_, accepted := s.projectRoutedEvent("tab", a, 0, app.SessionEventMsg{Projection: &app.PresentationState{}})
	assert.False(t, accepted)
	assert.Nil(t, s.GetRunner("tab").projection)
}
