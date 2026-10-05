package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionOwnerRegressionCanonicalCancelStrandsElicitation(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.phase = sessionRunning
		d.activeRequestID = "turn"
		d.generation++
		d.cancel = cancel
		return nil
	}))
	delivered := make(chan Event, 1)
	r.OnElicitationRequest(func(e Event) { delivered <- e })
	done := make(chan error, 1)
	go func() {
		_, err := r.requestElicitation(ctx, elicitationSpec{sessionID: s.ID, message: "question", mode: "form"})
		done <- err
	}()
	e := (<-delivered).(*ElicitationRequestEvent)
	var waiter *elicitationWaiter
	require.NoError(t, d.ownerCall(t.Context(), func() error { waiter = d.interactions[e.RequestID].waiter; return nil }))
	require.Equal(t, CancelAccepted, d.Cancel("turn"))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation stranded elicitation")
	}
	require.Equal(t, int32(waiterCanceled), waiter.state.Load())
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; d.cancel = nil; return nil }))
}

func TestSessionOwnerRegressionMaxIterationsReusesPromptIdentity(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.activeRequestID = "same-turn"; d.generation++; return nil }))
	emitted := make(chan Event, 8)
	sink := EventSinkFunc(func(e Event) { emitted <- e })
	run := func(iteration, limit int) <-chan iterationDecision {
		done := make(chan iterationDecision, 1)
		go func() {
			_, decision := r.enforceMaxIterations(t.Context(), s, r.currentAgent(), iteration, limit, sink)
			done <- decision
		}()
		return done
	}
	first := run(1, 1)
	e1 := (<-emitted).(*MaxIterationsReachedEvent)
	response := InteractionResponse{InteractionID: e1.RequestID, Kind: InteractionMaxIterations, Resume: ResumeApprove()}
	require.NoError(t, h.Respond(t.Context(), response))
	require.Equal(t, iterationContinue, <-first)
	second := run(11, 11)
	e2 := (<-emitted).(*MaxIterationsReachedEvent)
	require.NotEqual(t, e1.RequestID, e2.RequestID)
	require.Error(t, h.Respond(t.Context(), response))
	response.InteractionID = e2.RequestID
	require.NoError(t, h.Respond(t.Context(), response))
	require.Equal(t, iterationContinue, <-second)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; return nil }))
}

func TestSessionOwnerRegressionObservationMutatesCanonicalAdmission(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.activeRequestID = "turn"; d.generation++; return nil }))
	first, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer first.Cancel()
	second, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer second.Cancel()
	input := TurnInput{Content: "note", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "original"}}}}
	_, err = h.Steer(t.Context(), input)
	input.MultiContent[0].ImageURL.URL = "caller-changed"
	require.NoError(t, err)
	one := (<-first.Events).Event.(*PendingUserMessageAcceptedEvent)
	two := (<-second.Events).Event.(*PendingUserMessageAcceptedEvent)
	require.NotSame(t, one, two)
	one.MultiContent[0].ImageURL.URL = "observer-changed"
	snapshot, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "original", snapshot.Messages[0].Message.Message.MultiContent[0].ImageURL.URL)
	require.Equal(t, "original", two.MultiContent[0].ImageURL.URL)
	zero := uint64(0)
	replayed, err := h.Observe(t.Context(), ObserveOptions{Since: &zero, SinceEpoch: first.Primary().Epoch})
	require.NoError(t, err)
	defer replayed.Cancel()
	require.Len(t, replayed.Replay, 1)
	replay := replayed.Replay[0].Event.(*PendingUserMessageAcceptedEvent)
	require.Equal(t, "original", replay.MultiContent[0].ImageURL.URL)
	replay.MultiContent[0].ImageURL.URL = "replay-changed"
	snapshot, err = d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "original", snapshot.Messages[0].Message.Message.MultiContent[0].ImageURL.URL)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; return nil }))
}

func TestSessionOwnerRegressionOuterObservationBufferBypassesHubClamp(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{Buffer: 4 * maxSessionEventSubscriberBuffer})
	require.NoError(t, err)
	defer obs.Cancel()
	require.Equal(t, maxSessionEventSubscriberBuffer+1, cap(obs.Events))
}

func TestSessionOwnerRegressionObservationOuterQueueBypassesByteBudget(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	obs, err := h.Observe(t.Context(), ObserveOptions{Buffer: 16})
	require.NoError(t, err)
	defer obs.Cancel()
	for range 10 {
		d.events.Publish(s.ID, UserMessage(string(make([]byte, 1<<20)), s.ID, nil))
	}
	d.events.mu.Lock()
	count := d.events.subscriberCount
	d.events.mu.Unlock()
	require.Zero(t, count)
	gap := false
	for item := range obs.Events {
		gap = gap || item.Gap
	}
	require.True(t, gap)
}

func TestSessionOwnerRegressionElicitationResolutionPrecedesPrompt(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	r.OnElicitationRequest(func(raw Event) {
		e := raw.(*ElicitationRequestEvent)
		require.NoError(t, h.Respond(t.Context(), InteractionResponse{InteractionID: e.RequestID, Kind: InteractionElicitation, ElicitationID: e.ElicitationID, Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}}))
	})
	_, err = r.requestElicitation(t.Context(), elicitationSpec{sessionID: s.ID, message: "question", mode: "form"})
	require.NoError(t, err)
	first, second := <-obs.Events, <-obs.Events
	require.IsType(t, &ElicitationRequestEvent{}, first.Event)
	require.IsType(t, &InteractionResolvedEvent{}, second.Event)
	require.Equal(t, first.InteractionID, second.InteractionID)
	baseline, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer baseline.Cancel()
	require.Empty(t, baseline.Primary().Interactions)
}

func TestSessionInputDetachWithDurableStores(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if backend == "sqlite" {
				sqlite, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sqlite.Close()) })
				store = sqlite
			}
			r := newPersistedSessionRuntime(t, store)
			s := session.New(session.WithID(t.Name()), session.WithAgentName("root"))
			h, err := r.CreateSession(t.Context(), s, SessionBinding{})
			require.NoError(t, err)
			d := h.(*sessionHandle).driver
			require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.activeRequestID = "turn"; return nil }))
			input := TurnInput{Content: "note", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "original"}}}}
			_, err = h.Steer(t.Context(), input)
			require.NoError(t, err)
			input.MultiContent[0].ImageURL.URL = "mutated"
			stored, err := store.GetSession(t.Context(), s.ID)
			require.NoError(t, err)
			require.Equal(t, "original", stored.Messages[0].Message.Message.MultiContent[0].ImageURL.URL)
			owned, err := d.ownerSnapshot(t.Context())
			require.NoError(t, err)
			require.Equal(t, "original", owned.Messages[0].Message.Message.MultiContent[0].ImageURL.URL)
			require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; return nil }))
		})
	}
}

func TestConfirmationOccurrencesPreserveProviderIdentity(t *testing.T) {
	r, s, _, agentTools := makeJudgedRuntime(t, "ask", "approval required", nil)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	events := make(chan Event, 16)
	sink := EventSinkFunc(func(e Event) { events <- e })
	run := func() <-chan struct{} {
		done := make(chan struct{})
		go func() {
			r.processToolCalls(t.Context(), s, []tools.ToolCall{{ID: "repeated", Function: tools.FunctionCall{Name: "the_tool", Arguments: `{}`}}}, agentTools, sink)
			close(done)
		}()
		return done
	}
	prompt := func() *ToolCallConfirmationEvent {
		for {
			select {
			case e := <-events:
				if prompt, ok := e.(*ToolCallConfirmationEvent); ok {
					return prompt
				}
			case <-time.After(time.Second):
				t.Fatal("missing confirmation")
				return nil
			}
		}
	}
	first := run()
	one := prompt()
	require.Equal(t, "repeated", one.ToolCall.ID)
	require.NotEqual(t, one.ToolCall.ID, one.RequestID)
	response := InteractionResponse{InteractionID: one.RequestID, Kind: InteractionConfirmation, Resume: ResumeApprove()}
	require.NoError(t, h.Respond(t.Context(), response))
	<-first
	second := run()
	two := prompt()
	require.Equal(t, "repeated", two.ToolCall.ID)
	require.NotEqual(t, one.RequestID, two.RequestID)
	require.Error(t, h.Respond(t.Context(), response))
	response.InteractionID = two.RequestID
	require.NoError(t, h.Respond(t.Context(), response))
	<-second
}

func TestTreeObservationHasNoUnaccountedForwardingQueue(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	for _, ordered := range []bool{false, true} {
		obs, err := h.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: ordered, Buffer: 4 * maxSessionEventSubscriberBuffer})
		require.NoError(t, err)
		require.Zero(t, cap(obs.Events))
		require.Zero(t, cap(obs.SessionsAdded))
		require.Zero(t, cap(obs.TreeUpdates))
		obs.Cancel()
	}
}

func TestCanonicalStopTerminatesElicitationWithoutCallerCancellation(t *testing.T) {
	r, s := newSessionFixture(t)
	h, err := r.CreateSession(t.Context(), s, SessionBinding{})
	require.NoError(t, err)
	delivered := make(chan Event, 1)
	r.OnElicitationRequest(func(e Event) { delivered <- e })
	done := make(chan error, 1)
	go func() {
		_, err := r.requestElicitation(t.Context(), elicitationSpec{sessionID: s.ID, message: "question"})
		done <- err
	}()
	<-delivered
	h.(*sessionHandle).driver.StopAll()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("stop stranded elicitation")
	}
}
