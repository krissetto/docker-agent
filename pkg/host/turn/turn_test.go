package turn

import (
	"context"
	"errors"
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
	if ctx.Done() != nil {
		panic("observation must outlive caller")
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
