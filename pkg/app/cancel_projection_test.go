package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type projectionSession struct {
	runtime.UnsupportedSessionHandle

	mu           sync.Mutex
	id           string
	next         int
	events       chan runtime.SessionEvent
	errors       chan error
	activeTurnID string
	cancelTurnID string
	inputs       []runtime.TurnInput
	observe      func(context.Context, runtime.ObserveOptions) (runtime.Observation, error)
	observeTree  func(context.Context) (runtime.Observation, error)
}

func (a *projectionSession) ID() string      { return a.id }
func (*projectionSession) AgentName() string { return "root" }
func (a *projectionSession) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: a.id, AgentName: "root"}
}

func (a *projectionSession) Submit(_ context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	a.inputs = append(a.inputs, input)
	return runtime.Submission{SessionID: a.id, TurnID: string(rune('0' + a.next))}, nil
}

func (a *projectionSession) Retry(ctx context.Context) (runtime.Submission, error) {
	return a.Submit(ctx, runtime.TurnInput{Retry: true})
}

func (a *projectionSession) Steer(ctx context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	return a.Submit(ctx, input)
}

func (a *projectionSession) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	if options.Tree {
		if a.observeTree != nil {
			return a.observeTree(ctx)
		}
		return runtime.Observation{}, runtime.ErrUnsupported
	}
	if a.observe != nil {
		return a.observe(ctx, options)
	}
	return runtime.Observation{Events: a.events, Errors: a.errors, Cancel: func() {}}, nil
}

func (a *projectionSession) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{SessionID: a.id, State: runtime.SessionStateRunning}, nil
}
func (*projectionSession) Respond(context.Context, runtime.InteractionResponse) error { return nil }
func (a *projectionSession) Cancel(_ context.Context, turnID string) (runtime.CancelResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelTurnID = turnID
	outcome := runtime.CancelAccepted
	if a.activeTurnID != "" && turnID != a.activeTurnID {
		outcome = runtime.CancelNotActive
	}
	return runtime.CancelResult{SessionID: a.id, Outcome: outcome}, nil
}

type projectionSessions struct{ session *projectionSession }

func (r *projectionSessions) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return r.session, nil
}

func (r *projectionSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, Operation: "lookup"}
}
func (*projectionSessions) DeleteSession(context.Context, string) error { return nil }

func (a *projectionSession) Compact(_ context.Context, prompt string, sink runtime.EventSink) error {
	sink.Emit(runtime.SessionCompactionCompleted(a.id, runtime.CompactionOutcomeApplied, prompt))
	return nil
}

func TestAppCompactSessionBridgesSessionEvents(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("compact-app"), session.WithAgentName("root"))
	handle := &projectionSession{id: sess.ID}
	a := &App{currentState: sessionState{session: sess, handle: handle}, events: make(chan any, 1)}

	require.NoError(t, a.CompactSession(t.Context(), "focus"))
	event, ok := (<-a.events).(*runtime.SessionCompactionEvent)
	require.True(t, ok)
	assert.Equal(t, sess.ID, event.SessionID)
	assert.Equal(t, runtime.CompactionOutcomeApplied, event.Outcome)
}

func TestAppCompactSessionUnsupportedSession(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("compact-unsupported"), session.WithAgentName("root"))
	a := &App{currentState: sessionState{session: sess}, events: make(chan any, 1)}

	err := a.CompactSession(t.Context(), "")
	require.ErrorIs(t, err, runtime.ErrUnsupported)
}

func TestCancelResubmitProjectsEachAcceptedRequestExactlyOnce(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("projection"), session.WithAgentName("root"))
	handle := &projectionSession{id: sess.ID, events: make(chan runtime.SessionEvent, 16)}
	a := New(t.Context(), &projectionSessions{session: handle}, sess, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	require.True(t, a.startSessionEventBridge(t.Context()))

	ctx1, cancel1 := context.WithCancel(t.Context())
	a.Run(ctx1, cancel1, "first", nil)
	emitProjection(handle.events, "1", runtime.UserMessage("first", sess.ID, nil), runtime.StreamStarted(sess.ID, "root"))
	assertProjectedTypes(t, a.events, "user_message", "stream_started")

	a.MarkRunCancelled()
	cancel1()
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	a.Run(ctx2, cancel2, "second", nil)

	// The promoted request's canonical prompt/start may arrive before the old
	// cancelled stop. That stale stop must not hide or terminate request 2.
	emitProjection(handle.events, "2", runtime.UserMessage("second", sess.ID, nil), runtime.StreamStarted(sess.ID, "root"))
	assertProjectedTypes(t, a.events, "user_message", "stream_started")
	handle.events <- runtime.SessionEvent{TurnID: "1", TranscriptPosition: -1, Event: runtime.StreamStopped(sess.ID, "root", "cancelled")}
	handle.events <- runtime.SessionEvent{TurnID: "2", TranscriptPosition: -1, Event: runtime.AgentChoice("root", sess.ID, "ok")}
	handle.events <- runtime.SessionEvent{TurnID: "2", TranscriptPosition: -1, Event: runtime.StreamStopped(sess.ID, "root", "normal")}
	assertProjectedTypes(t, a.events, "agent_choice", "stream_stopped")

	assert.Empty(t, a.cancelledRequests)
	assert.Empty(t, a.projectedRequestID)
}

func TestCancelRunTargetsActiveProjectedRequestInsteadOfQueuedSubmission(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("cancel-active-projection"), session.WithAgentName("root"))
	handle := &projectionSession{id: sess.ID, events: make(chan runtime.SessionEvent, 16), activeTurnID: "1"}
	handle.observe = func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		return runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}}}, Events: handle.events, Cancel: func() {}}, nil
	}
	a := New(t.Context(), &projectionSessions{session: handle}, sess, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	require.True(t, a.startSessionEventBridge(t.Context()))
	initial, ok := (<-a.events).(SessionEventMsg)
	require.True(t, ok)
	require.IsType(t, &SessionResetEvent{}, initial.Event)

	a.Run(t.Context(), func() {}, "active", nil)
	// Only an owner-correlated, sequenced start grants canonical cancellation
	// authority; the legacy helper intentionally emits identity-free events.
	handle.events <- runtime.SessionEvent{SessionID: sess.ID, TurnID: "1", Sequence: 1, TranscriptPosition: -1, Event: runtime.UserMessage("active", sess.ID, nil)}
	handle.events <- runtime.SessionEvent{SessionID: sess.ID, TurnID: "1", Sequence: 2, TranscriptPosition: -1, Event: runtime.StreamStarted(sess.ID, "root")}
	assertProjectedTypes(t, a.events, "user_message", "stream_started")

	submission, err := a.FollowUpMessage(t.Context(), "queued", nil)
	require.NoError(t, err)
	require.Equal(t, "2", submission.TurnID)

	assert.Equal(t, runtime.CancelAccepted, a.CancelRun())
	assert.Equal(t, "1", handle.cancelTurnID)
}

func TestProjectionResetSeedsSnapshotTitle(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("title-projection"))
	a := New(t.Context(), nil, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: sess.ID}

	sink.Reset(runtime.SessionSnapshot{Session: session.New(session.WithID(sess.ID), session.WithTitle("generated before attach"))})

	msg := <-a.events
	bridged, ok := msg.(SessionEventMsg)
	require.True(t, ok)
	require.True(t, bridged.Seed)
	reset, ok := bridged.Event.(*SessionResetEvent)
	require.True(t, ok)
	assert.Equal(t, sess.ID, reset.GetSessionID())
	assert.Equal(t, "generated before attach", reset.Snapshot.Session.TitleSnapshot())
}

func TestProjectionResetRemovesPendingInputsMissingFromAuthoritativeSnapshot(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("resnapshot"))
	a := New(t.Context(), nil, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: sess.ID}

	sink.Reset(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID}, PendingInputs: []runtime.PendingInput{{TurnID: "a", Content: "first"}, {TurnID: "b", Content: "second"}}})
	first := (<-a.events).(SessionEventMsg)
	assert.Equal(t, []string{"a", "b"}, first.Projection.Lifecycle.Pending)

	sink.Reset(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID}, PendingInputs: []runtime.PendingInput{{TurnID: "b", Content: "second"}}})
	replacement := (<-a.events).(SessionEventMsg)
	assert.Equal(t, []string{"b"}, replacement.Projection.Lifecycle.Pending)
	assert.Len(t, replacement.Event.(*SessionResetEvent).Snapshot.PendingInputs, 1)
}

func emitProjection(out chan<- runtime.SessionEvent, requestID string, events ...runtime.Event) {
	for _, event := range events {
		out <- runtime.SessionEvent{TurnID: requestID, TranscriptPosition: -1, Event: event}
	}
}

func assertProjectedTypes(t *testing.T, in <-chan any, want ...string) {
	t.Helper()
	for _, typ := range want {
		msg := <-in
		bridged, ok := msg.(SessionEventMsg)
		require.True(t, ok)
		switch event := bridged.Event.(type) {
		case *runtime.UserMessageEvent:
			assert.Equal(t, typ, event.Type)
		case *runtime.StreamStartedEvent:
			assert.Equal(t, typ, event.Type)
		case *runtime.AgentChoiceEvent:
			assert.Equal(t, typ, event.Type)
		case *runtime.StreamStoppedEvent:
			assert.Equal(t, typ, event.Type)
		default:
			t.Fatalf("unexpected projected event %T", event)
		}
	}
}

func (a *projectionSession) Release(context.Context) error { return nil }

func (a *projectionSession) UpdateTitle(context.Context, string) error { return nil }

// A retryable transport loss is a connection state, not a session error:
// accepted work continues on the owner while the observer reconnects.
func TestAppProjectionTransportLossReportsReconnecting(t *testing.T) {
	sess := session.New()
	events := make(chan runtime.SessionEvent)
	errs := make(chan error, 1)
	errs <- errors.New("transport dropped")
	close(errs)
	close(events)
	handle := &projectionSession{id: sess.ID, events: events, errors: errs}
	a := New(t.Context(), &projectionSessions{session: handle}, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	got := make(chan any, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.SubscribeWith(ctx, func(msg tea.Msg) { got <- msg })
	a.Start(ctx)
	require.Eventually(t, func() bool {
		select {
		case msg := <-got:
			event, ok := msg.(*ConnectionStateEvent)
			return ok && event.State == ConnectionReconnecting && event.Err == "transport dropped"
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}
