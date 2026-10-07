package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunTurnRemoteReconnectNeverResubmitsAcceptedTurn(t *testing.T) {
	var submits, observes atomic.Int32
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			submits.Add(1)
			close(accepted)
			fmt.Fprint(w, `{"session_id":"s","turn_id":"accepted"}`)
			return
		}
		n := observes.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"epoch\":\"authority-epoch\",\"cursor\":%d}}\n\n", n-1)
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"ready\",\"epoch\":\"authority-epoch\",\"cursor\":%d}\n\n", n-1)
		w.(http.Flusher).Flush()
		<-accepted
		if n > 1 {
			assert.Equal(t, fmt.Sprint(n-1), r.URL.Query().Get("since"))
			assert.Equal(t, "authority-epoch", r.URL.Query().Get("since_epoch"))
		}
		event := `{"type":"agent_choice","session_id":"s","content":"piece"}`
		if n == 7 {
			event = `{"type":"stream_stopped","session_id":"s","reason":"normal"}`
		}
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"turn_id\":\"accepted\",\"sequence\":%d,\"epoch\":\"authority-epoch\",\"event\":%s}}\n\n", n, event)
		if n < 7 {
			// Cut a following frame mid-line. Reconnect must replay from the
			// last complete event without submitting the accepted turn again.
			fmt.Fprint(w, `data: {"version":2,"type":"event","envelope":`)
		}
	}))
	defer server.Close()
	remote, err := runtime.NewClient(server.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(remote)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var output []uint64
	result, err := RunTurnFunc(ctx, handle, runtime.TurnInput{Content: "once"}, func(_ context.Context, e runtime.SessionEvent) error { output = append(output, e.Sequence); return nil })
	require.NoError(t, err)
	assert.Equal(t, "accepted", result.Submission.TurnID)
	assert.Equal(t, int32(1), submits.Load())
	assert.Equal(t, int32(7), observes.Load())
	assert.Equal(t, []uint64{1, 2, 3, 4, 5, 6, 7}, output)
}

func TestAcceptedTurnReplayDedupesAndGapDoesNotRetryMutation(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	errs := make(chan error)
	close(errs)
	initial := obs(0, []runtime.SessionEvent{{TurnID: "accepted", Sequence: 1}}, events)
	initial.Errors = errs
	calls := 0
	observer := observerFunc(func(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		require.Equal(t, uint64(1), *options.Since)
		next := obs(2, []runtime.SessionEvent{{TurnID: "accepted", Sequence: 1}, {TurnID: "accepted", Sequence: 2}, {Gap: true}}, events)
		next.Errors = errs
		return next, nil
	})
	var output []uint64
	result := consumeAcceptedTurn(t.Context(), observer, initial, "accepted", func(_ context.Context, e runtime.SessionEvent) (TurnDecision, error) {
		output = append(output, e.Sequence)
		return TurnContinue, nil
	}, observationRetryPolicy{now: time.Now, wait: func(context.Context, int) bool { return true }})
	var gap *ObservationGapError
	require.ErrorAs(t, result.Err, &gap)
	assert.Equal(t, 1, calls)
	assert.Equal(t, []uint64{1, 2}, output)
}

func TestConsumeTurnDrainsFinalQueuedStopBeforeTransportFailure(t *testing.T) {
	for range 30 {
		events := make(chan runtime.SessionEvent, 1)
		events <- runtime.SessionEvent{TurnID: "accepted", Sequence: 1, Event: runtime.StreamStopped("s", "root", "normal")}
		close(events)
		errs := make(chan error, 1)
		errs <- ErrTurnObservationEnded
		close(errs)
		result := ConsumeTurn(t.Context(), runtime.Observation{Events: events, Errors: errs}, "accepted", func(context.Context, runtime.SessionEvent) (TurnDecision, error) { return TurnContinue, nil })
		require.True(t, result.Stopped)
		require.NoError(t, result.Err)
	}
}

func TestAcceptedTurnEpochChangeIsTerminal(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	errs := make(chan error)
	close(errs)
	initial := obs(7, nil, events)
	initial.Initial[0].Epoch = "old"
	initial.Errors = errs
	var detached atomic.Int32
	calls := 0
	observer := observerFunc(func(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		require.Equal(t, "old", options.SinceEpoch)
		require.Equal(t, uint64(7), *options.Since)
		next := obs(8, []runtime.SessionEvent{{TurnID: "accepted", Sequence: 8, Event: runtime.StreamStopped("s", "root", "normal")}}, events)
		next.Initial[0].Epoch = "new"
		next.Cancel = func() { detached.Add(1) }
		return next, nil
	})
	result := consumeAcceptedTurn(t.Context(), observer, initial, "accepted", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
		t.Fatal("must not apply another authority's output")
		return TurnContinue, nil
	}, observationRetryPolicy{now: time.Now, wait: func(context.Context, int) bool { return true }})
	var discontinuity *ObservationDiscontinuityError
	require.ErrorAs(t, result.Err, &discontinuity)
	assert.Equal(t, 1, calls)
	assert.Equal(t, int32(1), detached.Load())
}

func TestRunTurnRemoteReconnectRecoversInteractionAndStopReplay(t *testing.T) {
	var submits, observes, responses, cancels atomic.Int32
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			submits.Add(1)
			close(accepted)
			fmt.Fprint(w, `{"session_id":"s","turn_id":"accepted"}`)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			responses.Add(1)
			var response struct {
				InteractionID string `json:"interaction_id"`
			}
			// The wire response deliberately retains the interaction's identity.
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&response)) {
				http.Error(w, "invalid response", http.StatusBadRequest)
				return
			}
			assert.Equal(t, "approval", response.InteractionID)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			cancels.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			n := observes.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			cursor := 0
			if n > 1 {
				cursor = 3
				assert.Equal(t, "owner", r.URL.Query().Get("since_epoch"))
				assert.Equal(t, "1", r.URL.Query().Get("since"))
			}
			fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"epoch\":\"owner\",\"cursor\":%d}}\n\n", cursor)
			if n > 1 {
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"turn_id\":\"accepted\",\"interaction_id\":\"approval\",\"sequence\":2,\"epoch\":\"owner\",\"event\":{\"type\":\"tool_call_confirmation\",\"tool_call\":{\"id\":\"tool\"}}}}\n\n")
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"turn_id\":\"accepted\",\"sequence\":3,\"epoch\":\"owner\",\"event\":{\"type\":\"stream_stopped\",\"session_id\":\"s\",\"reason\":\"normal\"}}}\n\n")
			}
			fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":%d}\n\n", cursor)
			w.(http.Flusher).Flush()
			if n == 1 {
				select {
				case <-accepted:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"turn_id\":\"accepted\",\"sequence\":1,\"epoch\":\"owner\",\"event\":{\"type\":\"agent_choice\",\"session_id\":\"s\",\"content\":\"once\"}}}\n\ndata: {")
			}
		}
	}))
	defer server.Close()
	remote, err := runtime.NewClient(server.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(remote)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	var output []uint64
	_, err = RunTurnFunc(t.Context(), handle, runtime.TurnInput{Content: "once"}, func(_ context.Context, event runtime.SessionEvent) error {
		output = append(output, event.Sequence)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 3}, output)
	assert.Equal(t, int32(1), submits.Load())
	assert.Equal(t, int32(2), observes.Load())
	assert.Equal(t, int32(1), responses.Load())
	assert.Zero(t, cancels.Load())
}

type detachOnlyTurnHandle struct {
	*turnHandleStub

	cancels atomic.Int32
	cancel  context.CancelFunc
}

func (s *detachOnlyTurnHandle) Cancel(context.Context, string) (runtime.CancelResult, error) {
	s.cancels.Add(1)
	return runtime.CancelResult{}, nil
}

func (s *detachOnlyTurnHandle) Submit(ctx context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	submission, err := s.turnHandleStub.Submit(ctx, input)
	s.cancel()
	return submission, err
}

func TestRunTurnCallerCancellationOnlyDetaches(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	handle := &detachOnlyTurnHandle{turnHandleStub: &turnHandleStub{observation: runtime.Observation{Events: make(chan runtime.SessionEvent)}}, cancel: cancel}
	_, err := RunTurnFunc(ctx, handle, runtime.TurnInput{}, func(context.Context, runtime.SessionEvent) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, handle.cancels.Load())
	assert.Equal(t, []string{"observe", "submit", "cancel"}, handle.calls)
}

func TestObservationConsumersRejectEnvelopeDiscontinuityBeforeFiltering(t *testing.T) {
	for _, event := range []runtime.SessionEvent{
		{Version: 2, Epoch: "other", Sequence: 5, TurnID: "unrelated"},
		{Version: 2, Sequence: 6, TurnID: "accepted"},
		{Sequence: 6, TurnID: "accepted"},
		{Epoch: "other", TurnID: "accepted", Event: runtime.StreamStopped("s", "root", "normal")},
	} {
		for _, replay := range []bool{true, false} {
			t.Run(fmt.Sprintf("epoch=%q/sequence=%d/replay=%t", event.Epoch, event.Sequence, replay), func(t *testing.T) {
				makeObservation := func() runtime.Observation {
					events := make(chan runtime.SessionEvent, 1)
					observation := obs(5, nil, events)
					observation.Initial[0].Epoch = "owner"
					if replay {
						observation.Replay = []runtime.SessionEvent{event}
					} else {
						events <- event
					}
					close(events)
					errs := make(chan error)
					close(errs)
					observation.Errors = errs
					return observation
				}
				sink := &recordingSink{}
				projection := projectObservation(t.Context(), sink, makeObservation(), nil)
				require.True(t, projection.gap)
				assert.Equal(t, uint64(5), projection.cursor)
				assert.Empty(t, sink.applied)

				observation := makeObservation()
				detaches := 0
				observation.Cancel = func() { detaches++ }
				observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
					t.Fatal("discontinuity must not reconnect an accepted turn")
					return runtime.Observation{}, nil
				})
				termination := consumeAcceptedTurn(t.Context(), observer, observation, "accepted", func(context.Context, runtime.SessionEvent) (TurnDecision, error) {
					t.Fatal("discontinuous envelope must not reach handler")
					return TurnContinue, nil
				}, observationRetryPolicy{now: time.Now, wait: func(context.Context, int) bool {
					t.Fatal("discontinuity must not back off")
					return false
				}})
				var discontinuity *ObservationDiscontinuityError
				require.ErrorAs(t, termination.Err, &discontinuity)
				assert.Equal(t, "owner", discontinuity.PreviousEpoch)
				assert.Equal(t, event.Epoch, discontinuity.CurrentEpoch)
				assert.Equal(t, uint64(5), discontinuity.Cursor)
				assert.Equal(t, 1, detaches)
			})
		}
	}
}

func TestObservationConsumersShareReplayCursorAndLegacyEpochPolicy(t *testing.T) {
	for _, version := range []int{0, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			epoch := "owner"
			if version == 0 {
				epoch = "" // Legacy observations and envelopes both omit authority.
			}
			events := make(chan runtime.SessionEvent)
			close(events)
			observation := obs(5, []runtime.SessionEvent{
				{Version: version, Epoch: epoch, Sequence: 5, TurnID: "accepted"},
				{Version: version, Epoch: epoch, Sequence: 6, TurnID: "unrelated"},
				{Version: version, Epoch: epoch, Sequence: 6, TurnID: "accepted"},
				{Version: version, Epoch: epoch, TurnID: "accepted"},
				{Version: version, Epoch: epoch, Sequence: 7, TurnID: "accepted", Event: runtime.StreamStopped("s", "root", "normal")},
			}, events)
			observation.Initial[0].Epoch = epoch
			sink := &recordingSink{}
			projection := projectObservation(t.Context(), sink, observation, nil)
			assert.False(t, projection.gap)
			assert.Equal(t, []uint64{6, 0, 7}, sink.applied)
			assert.Equal(t, uint64(7), projection.cursor)

			var delivered []uint64
			cursor := uint64(5)
			termination := consumeTurn(t.Context(), observation, "accepted", func(_ context.Context, event runtime.SessionEvent) (TurnDecision, error) {
				delivered = append(delivered, event.Sequence)
				return TurnContinue, nil
			}, &cursor, nil)
			require.NoError(t, termination.Err)
			assert.True(t, termination.Stopped)
			assert.Equal(t, []uint64{0, 7}, delivered)
			assert.Equal(t, projection.cursor, cursor)
		})
	}
}
