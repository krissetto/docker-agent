package client

import (
	"context"
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
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"cursor\":%d}}\n\n", n-1)
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":%d}\n\n", n-1)
		w.(http.Flusher).Flush()
		<-accepted
		if n > 1 {
			assert.Equal(t, fmt.Sprint(n-1), r.URL.Query().Get("since"))
		}
		event := `{"type":"agent_choice","session_id":"s","content":"piece"}`
		if n == 7 {
			event = `{"type":"stream_stopped","session_id":"s","reason":"normal"}`
		}
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"turn_id\":\"accepted\",\"sequence\":%d,\"event\":%s}}\n\n", n, event)
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
