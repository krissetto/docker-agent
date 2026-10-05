package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
)

func TestProjectionGapReplacesTranscriptStatusPendingAndInteractionsThroughFanout(t *testing.T) {
	a := newMetadataTestApp(t)
	first := session.New(session.WithID("s"), session.WithTitle("old"))
	first.AddMessage(session.UserMessage("old transcript"))
	replacement := session.New(session.WithID("s"), session.WithTitle("recovered"))
	replacement.AddMessage(session.UserMessage("missed committed transcript"))
	prompt := &runtime.MaxIterationsReachedEvent{SessionID: "s", RequestID: "live"}
	gap, live := make(chan runtime.SessionEvent, 1), make(chan runtime.SessionEvent)
	gap <- runtime.SessionEvent{Gap: true}
	count := 0
	handle := &projectionSession{id: "s", observe: func(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
		count++
		if count == 1 {
			return runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: first, Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateRunning}, Cursor: 1, TranscriptPosition: 1, PendingInputs: []runtime.PendingInput{{TurnID: "stale"}}, Interactions: []runtime.InteractionSnapshot{{Event: &runtime.MaxIterationsReachedEvent{SessionID: "s", RequestID: "stale"}}}}}, Events: gap, Cancel: func() {}}, nil
		}
		assert.Nil(t, options.Since)
		return runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: replacement, Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateSettled}, Cursor: 3, TranscriptPosition: 1, PendingInputs: []runtime.PendingInput{{TurnID: "new", Content: "pending"}}, Interactions: []runtime.InteractionSnapshot{{Event: prompt}}}}, Events: live, Cancel: func() {}}, nil
	}}
	got, ready := make(chan any, 8), make(chan struct{})
	go a.Subscribe(t.Context(), func(msg any) { got <- msg }, SubscribeOptions{PreserveSessionMetadata: true, Ready: ready})
	<-ready
	attachment, err := runtimeclient.Attach(t.Context(), handle, &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"})
	require.NoError(t, err)
	defer attachment.Detach()
	var reset SessionEventMsg
	for range 2 {
		select {
		case msg := <-got:
			reset = msg.(SessionEventMsg)
		case <-time.After(time.Second):
			t.Fatal("missing authoritative reset")
		}
	}
	snapshot := reset.Event.(*SessionResetEvent).Snapshot
	assert.Equal(t, "recovered", snapshot.Session.TitleSnapshot())
	assert.Equal(t, "missed committed transcript", snapshot.Session.GetLastUserMessageContent())
	assert.Equal(t, "missed committed transcript", a.Session().GetLastUserMessageContent())
	assert.Equal(t, runtime.SessionStateSettled, reset.Projection.Status.State)
	assert.Equal(t, "new", reset.Projection.PendingInputs[0].TurnID)
	assert.True(t, reset.Projection.HasInteraction(InteractionKey{SessionID: "s", InteractionID: "live"}))
	assert.False(t, reset.Projection.HasInteraction(InteractionKey{SessionID: "s", InteractionID: "stale"}))
}

func TestProjectionResolutionUsesExactSessionAndInteraction(t *testing.T) {
	a := newMetadataTestApp(t)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"}
	sink.Reset(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "s"}, Interactions: []runtime.InteractionSnapshot{
		{SessionID: "s", InteractionID: "same"}, {SessionID: "other", InteractionID: "same"}, {SessionID: "s", InteractionID: "live"},
	}})
	before := a.Presentation()
	sink.Apply(runtime.SessionEvent{Event: &runtime.InteractionResolvedEvent{SessionID: "s", InteractionID: "same"}})
	after := a.Presentation()
	assert.Len(t, before.Interactions, 3, "published heads are immutable")
	assert.False(t, after.HasInteraction(InteractionKey{SessionID: "s", InteractionID: "same"}))
	assert.True(t, after.HasInteraction(InteractionKey{SessionID: "other", InteractionID: "same"}))
	assert.True(t, after.HasInteraction(InteractionKey{SessionID: "s", InteractionID: "live"}))
	sink.Apply(runtime.SessionEvent{Event: runtime.AgentChoice("root", "s", "token")})
	assert.Same(t, after, a.Presentation(), "tokens do not clone presentation state")
}

func TestProjectionStaleResetCannotReplaceCurrentSession(t *testing.T) {
	a := newMetadataTestApp(t)
	a.bridgeEpoch.Store(2)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "old", epoch: 1}
	sink.Reset(runtime.SessionSnapshot{Session: session.New(session.WithID("old"))})
	assert.Nil(t, a.Session())
	assert.Nil(t, a.Presentation())
	assert.Empty(t, a.events)
}

func TestProjectionKeepsTypedHiddenInputsInCanonicalAccounting(t *testing.T) {
	a := newMetadataTestApp(t)
	a.projectEvent(&SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: session.New(session.WithID("s")), Status: runtime.SessionStatus{SessionID: "s"}}})
	for i, origin := range []session.InputOrigin{session.InputOriginRuntime, session.InputOriginAgent, session.InputOriginUser} {
		event := &runtime.PendingUserMessageAcceptedEvent{TurnID: string(rune('a' + i)), Message: "same", SessionPosition: i, InputOrigin: origin, SenderID: "child", SenderName: "worker", InputMode: "steer"}
		a.projectEvent(event)
		a.projectEvent(event)
	}
	assert.Equal(t, 3, a.Presentation().Status.Pending)
	assert.Len(t, a.Presentation().Lifecycle.Pending, 3)
	assert.Equal(t, 3, a.Session().ItemCount())
	for i, input := range a.Presentation().PendingInputs {
		msg := a.Session().ItemsSnapshot()[i].Message
		assert.Equal(t, input.InputOrigin, msg.InputOrigin)
		assert.Equal(t, input.SenderID, msg.SenderID)
		assert.Equal(t, input.SenderName, msg.SenderName)
		assert.Equal(t, input.InputMode, msg.InputMode)
	}
	assert.True(t, a.Session().ItemsSnapshot()[0].Message.Implicit)
	a.projectEvent(&runtime.PendingUserMessagePromotedEvent{TurnID: "a"})
	assert.Equal(t, 2, a.Presentation().Status.Pending)
	assert.Equal(t, 3, a.Session().ItemCount())
}

func TestProjectionPreservesLiveSeedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		envelope runtime.SessionEvent
		seed     bool
	}{
		{"live snapshot", runtime.SessionEvent{Version: 1, SessionID: "s", TranscriptPosition: -1}, true},
		{"remote snapshot", runtime.SessionEvent{Version: 2, SessionID: "s", TranscriptPosition: -1}, true},
		{"cursor replay", runtime.SessionEvent{Version: 1, SessionID: "s", Sequence: 9, TranscriptPosition: -1}, false},
		{"compatibility start", runtime.SessionEvent{SessionID: "s"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMetadataTestApp(t)
			sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"}
			tc.envelope.Event = runtime.StreamStarted("s", "worker")
			sink.Apply(tc.envelope)
			message := (<-a.events).(SessionEventMsg)
			assert.Equal(t, tc.seed, message.Seed)
			assert.Equal(t, tc.envelope.Sequence, message.Sequence)
		})
	}
}

func TestProjectionOrderedElicitationResolutionDoesNotResurrectPrompt(t *testing.T) {
	a := newMetadataTestApp(t)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"}
	sink.Reset(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "s"}})
	prompt := &runtime.ElicitationRequestEvent{Type: "elicitation_request", SessionID: "s", RequestID: "occurrence", ElicitationID: "elicitation"}
	sink.Apply(runtime.SessionEvent{Version: 1, SessionID: "s", Sequence: 1, InteractionID: prompt.RequestID, Event: prompt})
	require.True(t, a.Presentation().HasInteraction(InteractionKey{SessionID: "s", InteractionID: prompt.RequestID}))
	sink.Apply(runtime.SessionEvent{Version: 1, SessionID: "s", Sequence: 2, InteractionID: prompt.RequestID, Event: &runtime.InteractionResolvedEvent{Type: "interaction_resolved", SessionID: "s", InteractionID: prompt.RequestID, Reason: runtime.InteractionResponded}})
	require.Empty(t, a.Presentation().Interactions)
}
