package turn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
)

type testHandle struct {
	events    chan runtime.SessionEvent
	settled   chan struct{}
	cancelled chan string
	detached  atomic.Int32
	awaitErr  error
	submitErr error
}

func (h *testHandle) Observe(ctx context.Context, _ runtime.ObserveOptions) (runtime.Observation, error) {
	if ctx.Done() == nil {
		panic("observation establishment must be bounded")
	}
	return runtime.Observation{Events: h.events, Cancel: func() { h.detached.Add(1) }}, nil
}

func (h *testHandle) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{TurnID: "owned"}, h.submitErr
}

func (h *testHandle) Cancel(ctx context.Context, id string) (runtime.CancelResult, error) {
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		panic("cleanup must have an independent deadline")
	}
	h.cancelled <- id
	return runtime.CancelResult{Outcome: runtime.CancelAccepted}, nil
}

func (h *testHandle) AwaitTurn(ctx context.Context, id string) error {
	if id != "owned" {
		panic("waiting for another turn")
	}
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		panic("drain must have an independent deadline")
	}
	if h.awaitErr != nil {
		return h.awaitErr
	}
	select {
	case <-h.settled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newTestHandle() *testHandle {
	return &testHandle{events: make(chan runtime.SessionEvent, 2), settled: make(chan struct{}), cancelled: make(chan string, 2)}
}

func TestOwnedTurnDrainsOnEveryExit(t *testing.T) {
	for _, exit := range []string{"cancel", "handler", "gap", "eof", "normal"} {
		t.Run(exit, func(t *testing.T) {
			t.Parallel()
			h := newTestHandle()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			owned, err := Start(ctx, h, runtime.TurnInput{})
			require.NoError(t, err)
			switch exit {
			case "cancel":
				cancel()
			case "handler":
				h.events <- runtime.SessionEvent{TurnID: "owned", Event: &runtime.AgentChoiceEvent{}}
			case "gap":
				h.events <- runtime.SessionEvent{TurnID: "unrelated", Gap: true}
			case "eof":
				close(h.events)
			case "normal":
				h.events <- runtime.SessionEvent{TurnID: "owned", Event: &runtime.StreamStoppedEvent{}}
			}
			done := make(chan runtimeclient.TurnTermination, 1)
			go func() {
				done <- owned.Consume(ctx, func(context.Context, runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
					return runtimeclient.TurnTerminate, nil
				})
			}()
			if exit != "normal" {
				select {
				case id := <-h.cancelled:
					assert.Equal(t, "owned", id)
				case <-time.After(time.Second):
					t.Fatal("accepted request was not cancelled")
				}
			}
			require.Eventually(t, func() bool { return h.detached.Load() == 1 }, time.Second, time.Millisecond)
			select {
			case <-done:
				t.Fatal("returned before the driver settled")
			default:
			}
			close(h.settled)
			select {
			case result := <-done:
				if exit == "gap" {
					var gap *runtimeclient.ObservationGapError
					require.ErrorAs(t, result.Err, &gap, "host returns actionable gap only after cancellation and canonical settlement")
				}
				if exit == "normal" {
					require.True(t, result.Stopped)
					require.NoError(t, result.Err)
				}
			case <-time.After(time.Second):
				t.Fatal("settled request was not released")
			}
			assert.Empty(t, h.cancelled, "natural completion and completed cleanup must not cancel any successor")
		})
	}
}

func TestOwnedTurnReportsFailedSettlement(t *testing.T) {
	t.Parallel()
	h := newTestHandle()
	h.awaitErr = context.DeadlineExceeded
	h.events <- runtime.SessionEvent{TurnID: "owned", Event: &runtime.StreamStoppedEvent{}}
	owned, err := Start(t.Context(), h, runtime.TurnInput{})
	require.NoError(t, err)
	result := owned.Consume(t.Context(), func(context.Context, runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
		return runtimeclient.TurnContinue, nil
	})
	var drainErr *DrainError
	require.ErrorAs(t, result.Err, &drainErr)
	require.ErrorIs(t, drainErr, context.DeadlineExceeded)
	assert.Empty(t, h.cancelled)
}

func TestFailedAdmissionOnlyDetachesObservation(t *testing.T) {
	t.Parallel()
	h := newTestHandle()
	h.submitErr = errors.New("not admitted")
	owned, err := Start(t.Context(), h, runtime.TurnInput{})
	require.ErrorIs(t, err, h.submitErr)
	require.Nil(t, owned)
	assert.Equal(t, int32(1), h.detached.Load())
	assert.Empty(t, h.cancelled)
}

type blockingObserveHandle struct{ *testHandle }

func (h blockingObserveHandle) Observe(ctx context.Context, _ runtime.ObserveOptions) (runtime.Observation, error) {
	<-ctx.Done()
	return runtime.Observation{}, ctx.Err()
}

func TestStartObservationIsCallerBounded(t *testing.T) {
	h := blockingObserveHandle{newTestHandle()}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	owned, err := Start(ctx, h, runtime.TurnInput{})
	require.Nil(t, owned)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
	assert.Empty(t, h.cancelled)
}

type reconnectHandle struct {
	*testHandle

	observes       atomic.Int32
	epoch          string
	observationErr func() error
}

func (h *reconnectHandle) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	n := h.observes.Add(1)
	if n == 1 {
		h.observationErr = ctx.Err
		events := make(chan runtime.SessionEvent)
		close(events)
		errs := make(chan error)
		close(errs)
		return runtime.Observation{Initial: []runtime.SessionSnapshot{{Epoch: "old", Cursor: 3}}, Events: events, Errors: errs, Cancel: func() { h.detached.Add(1) }}, nil
	}
	if options.SinceEpoch != "old" || options.Since == nil || *options.Since != 3 {
		panic("reconnect lost authority cursor")
	}
	return runtime.Observation{Initial: []runtime.SessionSnapshot{{Epoch: h.epoch, Cursor: 3}}, Replay: []runtime.SessionEvent{{TurnID: "owned", Sequence: 4, Event: &runtime.StreamStoppedEvent{}}}, Cancel: func() { h.detached.Add(1) }}, nil
}

func TestOwnedTurnReconnectRetainsExactOwnership(t *testing.T) {
	for _, epoch := range []string{"old", "changed"} {
		t.Run(epoch, func(t *testing.T) {
			h := &reconnectHandle{testHandle: newTestHandle(), epoch: epoch}
			close(h.settled)
			admission, cancelAdmission := context.WithCancel(t.Context())
			owned, err := Start(admission, h, runtime.TurnInput{})
			require.NoError(t, err)
			cancelAdmission()
			require.NoError(t, h.observationErr(), "accepted observation must outlive admission context")
			result := owned.Consume(t.Context(), func(context.Context, runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
				return runtimeclient.TurnContinue, nil
			})
			if epoch == "old" {
				require.True(t, result.Stopped)
				require.NoError(t, result.Err)
				assert.Empty(t, h.cancelled)
			} else {
				var discontinuity *runtimeclient.ObservationDiscontinuityError
				require.ErrorAs(t, result.Err, &discontinuity)
				assert.Equal(t, "owned", <-h.cancelled)
			}
			assert.Equal(t, int32(2), h.observes.Load())
			assert.Equal(t, int32(2), h.detached.Load())
		})
	}
}

func TestStartRemoteHandshakeHonorsCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := runtime.NewClient(server.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	owned, err := Start(ctx, handle, runtime.TurnInput{})
	require.Nil(t, owned)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
}
