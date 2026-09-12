package client

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestConsumeTurnReplaysBeforeLiveAndFiltersOtherTurns(t *testing.T) {
	events := make(chan runtime.SessionEvent, 2)
	events <- runtime.SessionEvent{TurnID: "other", Sequence: 3}
	events <- runtime.SessionEvent{TurnID: "turn", Sequence: 4, Event: runtime.StreamStopped("s", "a", "normal")}
	close(events)
	cancelCalls := 0
	observation := runtime.Observation{
		Replay: []runtime.SessionEvent{
			{TurnID: "turn", Sequence: 1},
			{TurnID: "other", Sequence: 2},
		},
		Events: events,
		Cancel: func() { cancelCalls++ },
	}

	var sequences []uint64
	termination := ConsumeTurn(t.Context(), observation, "turn", func(_ context.Context, envelope runtime.SessionEvent) (TurnDecision, error) {
		sequences = append(sequences, envelope.Sequence)
		return TurnContinue, nil
	})

	require.NoError(t, termination.Err)
	assert.True(t, termination.Stopped)
	assert.Equal(t, []uint64{1, 4}, sequences)
	assert.Equal(t, 1, cancelCalls)
}

func TestConsumeTurnChecksGapBeforeTurnFilter(t *testing.T) {
	cancelCalls := 0
	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Replay: []runtime.SessionEvent{{TurnID: "other", Gap: true, FirstAvailable: 17}},
		Cancel: func() { cancelCalls++ },
	}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		t.Fatal("handler called for gap")
		return TurnContinue, nil
	})

	var gap *ObservationGapError
	require.ErrorAs(t, termination.Err, &gap)
	assert.Equal(t, uint64(17), gap.FirstAvailable)
	assert.False(t, termination.Stopped)
	assert.Equal(t, 1, cancelCalls)
}

func TestConsumeTurnCloseRacePrefersBufferedTransportError(t *testing.T) {
	for range 100 {
		events := make(chan runtime.SessionEvent)
		close(events)
		errs := make(chan error, 1)
		errs <- errors.New("transport dropped")

		termination := ConsumeTurn(t.Context(), runtime.Observation{Events: events, Errors: errs, Cancel: func() {}}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
			return TurnContinue, nil
		})
		assert.EqualError(t, termination.Err, "transport dropped")
	}
}

func TestConsumeTurnSurfacesObservationErrorAfterPrematureClose(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	errs := make(chan error, 1)
	errs <- errors.New("transport dropped")
	close(errs)
	cancelCalls := 0

	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Events: events,
		Errors: errs,
		Cancel: func() { cancelCalls++ },
	}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		return TurnContinue, nil
	})

	assert.EqualError(t, termination.Err, "transport dropped")
	assert.False(t, termination.Stopped)
	assert.Equal(t, 1, cancelCalls)
}

func TestConsumeTurnDeliversStoppedBeforeTermination(t *testing.T) {
	stop := runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "normal")}
	called := false
	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Replay: []runtime.SessionEvent{stop},
		Cancel: func() {},
	}, "turn", func(_ context.Context, envelope runtime.SessionEvent) (TurnDecision, error) {
		called = envelope.Event == stop.Event
		return TurnTerminate, errors.New("stop callback failed")
	})

	assert.True(t, called)
	assert.EqualError(t, termination.Err, "stop callback failed")
	assert.True(t, termination.Stopped)
}

func TestConsumeTurnSurfacesObservationErrorWhileEventsRemainOpen(t *testing.T) {
	errs := make(chan error, 1)
	errs <- errors.New("transport dropped")

	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Events: make(chan runtime.SessionEvent),
		Errors: errs,
		Cancel: func() {},
	}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		return TurnContinue, nil
	})

	assert.EqualError(t, termination.Err, "transport dropped")
	assert.True(t, termination.ObservationError)
}

func TestConsumeTurnPrematureCloseDoesNotWaitForErrors(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)

	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Events: events,
		Errors: make(chan error),
		Cancel: func() {},
	}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		return TurnContinue, nil
	})

	assert.ErrorIs(t, termination.Err, ErrTurnObservationEnded)
	assert.True(t, termination.ObservationError)
}

func TestConsumeTurnContextCancellationTerminates(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelCalls := 0

	termination := ConsumeTurn(ctx, runtime.Observation{
		Events: events,
		Cancel: func() { cancelCalls++ },
	}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		return TurnContinue, nil
	})

	assert.ErrorIs(t, termination.Err, context.Canceled)
	assert.True(t, termination.Terminated)
	assert.Equal(t, 1, cancelCalls)
}

func TestRunTurnObservesBeforeSubmitAndCancelsOnSubmitFailure(t *testing.T) {
	handle := &turnHandleStub{submitErr: errors.New("rejected")}
	_, err := RunTurnFunc(t.Context(), handle, runtime.TurnInput{Content: "hello"}, func(context.Context, runtime.SessionEvent) error {
		return nil
	})

	assert.EqualError(t, err, "submit turn: rejected")
	assert.Equal(t, []string{"observe", "submit", "cancel"}, handle.calls)
}

func TestRunTurnHandlesInteractionsByDefault(t *testing.T) {
	tests := []struct {
		name  string
		event runtime.Event
		kind  runtime.InteractionKind
	}{
		{name: "confirmation", event: runtime.ToolCallConfirmation(tools.ToolCall{}, tools.Tool{}, "a", nil), kind: runtime.InteractionConfirmation},
		{name: "max iterations", event: runtime.MaxIterationsReached(3), kind: runtime.InteractionMaxIterations},
		{name: "elicitation", event: runtime.ElicitationRequest("input", "form", nil, "", "elicitation", "", "s", nil, "a"), kind: runtime.InteractionElicitation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan runtime.SessionEvent, 2)
			events <- runtime.SessionEvent{TurnID: "turn", InteractionID: "interaction", Event: tc.event}
			events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "normal")}
			close(events)
			handle := &turnHandleStub{observation: runtime.Observation{Events: events}}

			result, err := RunTurn(t.Context(), handle, runtime.TurnInput{})

			require.NoError(t, err)
			assert.Equal(t, "normal", result.StopReason)
			require.Len(t, handle.responses, 1)
			assert.Equal(t, tc.kind, handle.responses[0].Kind)
			assert.Equal(t, "interaction", handle.responses[0].InteractionID)
			switch tc.kind {
			case runtime.InteractionConfirmation, runtime.InteractionMaxIterations:
				assert.Equal(t, runtime.ResumeTypeReject, handle.responses[0].Resume.Type)
			case runtime.InteractionElicitation:
				assert.Equal(t, "elicitation", handle.responses[0].ElicitationID)
				assert.Equal(t, tools.ElicitationActionDecline, handle.responses[0].Elicitation.Action)
			}
		})
	}
}

func TestRunTurnFuncRendersOrdinaryEventsAndDefaultsInteractions(t *testing.T) {
	events := make(chan runtime.SessionEvent, 3)
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.AgentChoice("a", "s", "hello")}
	events <- runtime.SessionEvent{TurnID: "turn", InteractionID: "interaction", Event: runtime.MaxIterationsReached(3)}
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "normal")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}
	var callbackEvents []runtime.Event

	_, err := RunTurnFunc(t.Context(), handle, runtime.TurnInput{}, func(_ context.Context, envelope runtime.SessionEvent) error {
		callbackEvents = append(callbackEvents, envelope.Event)
		return nil
	})

	require.NoError(t, err)
	require.Len(t, handle.responses, 1)
	require.Len(t, callbackEvents, 2)
	_, choice := callbackEvents[0].(*runtime.AgentChoiceEvent)
	_, stopped := callbackEvents[1].(*runtime.StreamStoppedEvent)
	assert.True(t, choice)
	assert.True(t, stopped)
}

func TestRunTurnFuncOwnedInteractionsCallbackRespondsExactlyOnce(t *testing.T) {
	events := make(chan runtime.SessionEvent, 2)
	events <- runtime.SessionEvent{TurnID: "turn", InteractionID: "interaction", Event: runtime.MaxIterationsReached(3)}
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "normal")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}

	_, err := RunTurnFuncOwnedInteractions(t.Context(), handle, runtime.TurnInput{}, func(ctx context.Context, envelope runtime.SessionEvent) error {
		if _, ok := envelope.Event.(*runtime.MaxIterationsReachedEvent); ok {
			return handle.Respond(ctx, runtime.InteractionResponse{InteractionID: envelope.InteractionID, Kind: runtime.InteractionMaxIterations, Resume: runtime.ResumeReject("")})
		}
		return nil
	})

	require.NoError(t, err)
	require.Len(t, handle.responses, 1)
	assert.Equal(t, "interaction", handle.responses[0].InteractionID)
}

func TestRunTurnReturnsRespondFailure(t *testing.T) {
	events := make(chan runtime.SessionEvent, 1)
	events <- runtime.SessionEvent{TurnID: "turn", InteractionID: "interaction", Event: runtime.MaxIterationsReached(3)}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}, respondErr: errors.New("write failed")}

	_, err := RunTurn(t.Context(), handle, runtime.TurnInput{})

	assert.EqualError(t, err, "respond to max_iterations interaction: write failed")
	assert.Equal(t, 1, countCalls(handle.calls, "cancel"))
}

func TestRunTurnErrorEventPrecedesCallback(t *testing.T) {
	events := make(chan runtime.SessionEvent, 1)
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.Error("boom")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}
	called := false

	_, err := RunTurnFunc(t.Context(), handle, runtime.TurnInput{}, func(context.Context, runtime.SessionEvent) error {
		called = true
		return errors.New("callback error")
	})

	var eventErr *TurnEventError
	require.ErrorAs(t, err, &eventErr)
	assert.False(t, called)
}

func TestRunTurnStopCallbackOrderingAndError(t *testing.T) {
	events := make(chan runtime.SessionEvent, 1)
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "normal")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}

	result, err := RunTurnFunc(t.Context(), handle, runtime.TurnInput{}, func(_ context.Context, envelope runtime.SessionEvent) error {
		_, stopped := envelope.Event.(*runtime.StreamStoppedEvent)
		assert.True(t, stopped)
		return errors.New("stop callback failed")
	})

	assert.Equal(t, "normal", result.StopReason)
	assert.EqualError(t, err, "stop callback failed")
	assert.Equal(t, 1, countCalls(handle.calls, "cancel"))
}

func TestRunTurnNilCallbackConsumesReplayAndCancelsOnce(t *testing.T) {
	handle := &turnHandleStub{observation: runtime.Observation{
		Replay: []runtime.SessionEvent{{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "replayed")}},
	}}

	result, err := RunTurn(t.Context(), handle, runtime.TurnInput{})

	require.NoError(t, err)
	assert.Equal(t, "replayed", result.StopReason)
	assert.Equal(t, 1, countCalls(handle.calls, "cancel"))
}

func countCalls(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func TestRunTurnDefaultErrorEventIsTypedError(t *testing.T) {
	events := make(chan runtime.SessionEvent, 2)
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.ErrorWithCode("model_failed", "boom")}
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "error")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}

	_, err := RunTurn(t.Context(), handle, runtime.TurnInput{})

	var eventErr *TurnEventError
	require.ErrorAs(t, err, &eventErr)
	assert.Equal(t, "model_failed", eventErr.Code)
	assert.Equal(t, "boom", eventErr.Message)
}

func TestRunTurnFuncThreeStatementUsage(t *testing.T) {
	events := make(chan runtime.SessionEvent, 2)
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.AgentChoice("a", "s", "hello")}
	events <- runtime.SessionEvent{TurnID: "turn", Event: runtime.StreamStopped("s", "a", "normal")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}
	var content string

	result, err := RunTurnFunc(t.Context(), handle, runtime.TurnInput{Content: "hi"}, func(_ context.Context, envelope runtime.SessionEvent) error {
		if event, ok := envelope.Event.(*runtime.AgentChoiceEvent); ok {
			content += event.Content
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "turn", result.Submission.TurnID)
	assert.Equal(t, "hello", content)
}

func TestRunTurnRequiresStreamStopped(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}

	_, err := RunTurn(t.Context(), handle, runtime.TurnInput{})

	assert.ErrorIs(t, err, ErrTurnObservationEnded)
}

func TestRunTurnCorrelatesThroughStopped(t *testing.T) {
	events := make(chan runtime.SessionEvent, 2)
	events <- runtime.SessionEvent{TurnID: "other", Sequence: 1}
	events <- runtime.SessionEvent{TurnID: "turn", Sequence: 2, Event: runtime.StreamStopped("s", "a", "normal")}
	close(events)
	handle := &turnHandleStub{observation: runtime.Observation{Events: events}}

	result, err := RunTurnFunc(t.Context(), handle, runtime.TurnInput{Content: "hello"}, func(context.Context, runtime.SessionEvent) error {
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, "turn", result.Submission.TurnID)
	assert.Equal(t, []string{"observe", "submit", "cancel"}, handle.calls)
}

type turnHandleStub struct {
	runtime.UnsupportedSessionHandle
	sessionStub
	observation runtime.Observation
	submitErr   error
	respondErr  error
	responses   []runtime.InteractionResponse
	calls       []string
}

func (s *turnHandleStub) Observe(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
	s.calls = append(s.calls, "observe")
	o := s.observation
	o.Cancel = func() { s.calls = append(s.calls, "cancel") }
	return o, nil
}

func (s *turnHandleStub) Respond(_ context.Context, response runtime.InteractionResponse) error {
	s.calls = append(s.calls, "respond")
	s.responses = append(s.responses, response)
	return s.respondErr
}

func (s *turnHandleStub) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	s.calls = append(s.calls, "submit")
	return runtime.Submission{TurnID: "turn"}, s.submitErr
}

func TestConsumeTurnPreservesHandlerTermination(t *testing.T) {
	events := make(chan runtime.SessionEvent, 1)
	events <- runtime.SessionEvent{TurnID: "turn", Sequence: 2}
	cancelCalls := 0
	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Replay: []runtime.SessionEvent{{TurnID: "turn", Sequence: 1}},
		Events: events,
		Cancel: func() { cancelCalls++ },
	}, "turn", func(_ context.Context, envelope runtime.SessionEvent) (TurnDecision, error) {
		assert.Equal(t, uint64(1), envelope.Sequence)
		return TurnTerminate, nil
	})

	require.NoError(t, termination.Err)
	assert.False(t, termination.Stopped)
	assert.Equal(t, 1, cancelCalls)
}

func TestRunTurnRejectsTreeObservationBeforeSubmit(t *testing.T) {
	handle := &turnHandleStub{observation: runtime.Observation{
		Initial:       []runtime.SessionSnapshot{{}, {}},
		SessionsAdded: make(chan runtime.SessionSnapshot),
	}}

	_, err := RunTurn(t.Context(), handle, runtime.TurnInput{})
	var treeErr *TreeObservationError
	require.ErrorAs(t, err, &treeErr)
	assert.Equal(t, []string{"observe", "cancel"}, handle.calls)
}

func TestConsumeTurnRejectsTreeObservation(t *testing.T) {
	termination := ConsumeTurn(t.Context(), runtime.Observation{
		Initial:       []runtime.SessionSnapshot{{}},
		SessionsAdded: make(chan runtime.SessionSnapshot),
		Cancel:        func() {},
	}, "turn", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		t.Fatal("tree events must not be consumed")
		return TurnContinue, nil
	})
	var treeErr *TreeObservationError
	assert.ErrorAs(t, termination.Err, &treeErr)
	assert.True(t, termination.ObservationError)
}
