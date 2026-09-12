package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type projectionSession struct {
	runtime.UnsupportedSessionHandle
	mu           sync.Mutex
	observations chan runtime.Observation
	observed     int
}

func (a *projectionSession) ID() string                        { return "session" }
func (a *projectionSession) AgentName() string                 { return "agent" }
func (a *projectionSession) Metadata() runtime.SessionMetadata { return runtime.SessionMetadata{} }
func (a *projectionSession) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (a *projectionSession) Retry(context.Context) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (a *projectionSession) Steer(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (a *projectionSession) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{}, nil
}

func (a *projectionSession) Respond(context.Context, runtime.InteractionResponse) error {
	return nil
}

func (a *projectionSession) Cancel(context.Context, string) (runtime.CancelResult, error) {
	return runtime.CancelResult{}, nil
}
func (a *projectionSession) Release(context.Context) error           { return nil }
func (*projectionSession) UpdateTitle(context.Context, string) error { return nil }
func (a *projectionSession) Observe(ctx context.Context, _ runtime.ObserveOptions) (runtime.Observation, error) {
	a.mu.Lock()
	a.observed++
	a.mu.Unlock()
	select {
	case observation := <-a.observations:
		return observation, nil
	case <-ctx.Done():
		return runtime.Observation{}, ctx.Err()
	}
}

func observation(snapshot runtime.SessionSnapshot, replay []runtime.SessionEvent, events chan runtime.SessionEvent) runtime.Observation {
	var once sync.Once
	return runtime.Observation{Initial: []runtime.SessionSnapshot{snapshot}, Replay: replay, Events: events, Cancel: func() { once.Do(func() {}) }}
}

func waitTabs(t *testing.T, s *Supervisor, check func(messages.TabInfo) bool) messages.TabInfo {
	t.Helper()
	var tab messages.TabInfo
	require.Eventually(t, func() bool {
		tabs, _ := s.GetTabs()
		if len(tabs) != 1 {
			return false
		}
		tab = tabs[0]
		return check(tab)
	}, time.Second, time.Millisecond)
	return tab
}

func TestProjectionSeedsLateOpenRunningTitleAndInteraction(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	handle := &projectionSession{observations: make(chan runtime.Observation, 1)}
	events := make(chan runtime.SessionEvent)
	prompt := runtime.ElicitationRequest("input", "form", nil, "", "e", "", "session", nil, "agent")
	handle.observations <- observation(runtime.SessionSnapshot{
		Session:      session.New(session.WithID("session"), session.WithTitle("canonical")),
		Status:       runtime.SessionStatus{State: runtime.SessionStateRunning},
		Interactions: []runtime.InteractionSnapshot{{Event: prompt}},
	}, nil, events)

	s.replaceProjection(t.Context(), "tab", handle)
	tab := waitTabs(t, s, func(tab messages.TabInfo) bool { return tab.Title == "canonical" })
	assert.Equal(t, messages.TabActivityRunning, tab.Activity)
	assert.True(t, tab.NeedsAttention)
	assert.Equal(t, prompt, s.ConsumePendingEvent("tab"))
	close(events)
}

func TestProjectionGapReplacesSnapshotAndObserver(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "tab")
	handle := &projectionSession{observations: make(chan runtime.Observation, 2)}
	firstEvents := make(chan runtime.SessionEvent, 1)
	firstEvents <- runtime.SessionEvent{Gap: true}
	handle.observations <- observation(runtime.SessionSnapshot{Session: session.New(session.WithTitle("old")), Status: runtime.SessionStatus{State: runtime.SessionStateRunning}}, nil, firstEvents)
	secondEvents := make(chan runtime.SessionEvent)
	handle.observations <- observation(runtime.SessionSnapshot{Session: session.New(session.WithTitle("new")), Status: runtime.SessionStatus{State: runtime.SessionStateSettled}}, nil, secondEvents)

	s.replaceProjection(t.Context(), "tab", handle)
	tab := waitTabs(t, s, func(tab messages.TabInfo) bool { return tab.Title == "new" })
	assert.Equal(t, messages.TabActivityNone, tab.Activity)
	handle.mu.Lock()
	assert.Equal(t, 2, handle.observed)
	handle.mu.Unlock()
	close(secondEvents)
}

func TestCloseTabCancelsProjectionNotSession(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "tab")
	handle := &projectionSession{observations: make(chan runtime.Observation)}
	s.replaceProjection(t.Context(), "tab", handle)
	s.CloseSession("tab")
	assert.Empty(t, s.order)
	// The fake's session Cancel and Release methods are intentionally not observed:
	// closing a tab only cancels the projection context.
}

func TestSupervisorProjectionTransportErrorIsVisible(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	sink := &tabProjectionSink{s: s, tabID: "tab", generation: 0}
	sink.OnError(errors.New("transport failed"))

	tab := waitTabs(t, s, func(tab messages.TabInfo) bool { return tab.NeedsAttention })
	assert.True(t, tab.NeedsAttention)
}

func TestSupervisorProjectionRuntimeErrorNeedsInactiveTabAttention(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")
	sink := &tabProjectionSink{s: s, tabID: "tab", generation: 0}
	sink.Apply(runtime.SessionEvent{Event: runtime.Error("run failed")})

	tab := waitTabs(t, s, func(tab messages.TabInfo) bool { return tab.NeedsAttention })
	assert.True(t, tab.NeedsAttention)
}
