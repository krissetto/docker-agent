package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

type workflowHandle struct {
	runtime.UnsupportedSessionHandle
	sessionStub
	settled chan struct{}
}

func (h *workflowHandle) AwaitTurn(context.Context, string) error { return nil }
func (h *workflowHandle) AwaitWorkflow(context.Context) (runtime.WorkflowSettlement, error) {
	close(h.settled)
	return runtime.WorkflowSettlement{Epoch: "owner", Cursor: 4}, nil
}

func TestConsumeWorkflowDrainsFollowUpOutputAfterSettlement(t *testing.T) {
	handle := &workflowHandle{settled: make(chan struct{})}
	events := make(chan runtime.SessionEvent)
	observation := runtime.Observation{Initial: []runtime.SessionSnapshot{{Epoch: "owner"}}, Events: events, Cancel: func() {}}
	done := make(chan TurnTermination, 1)
	var rendered []string
	go func() {
		done <- ConsumeWorkflow(t.Context(), handle, observation, "original", func(_ context.Context, event runtime.SessionEvent) (TurnDecision, error) {
			if text, ok := event.Event.(*runtime.AgentChoiceEvent); ok {
				rendered = append(rendered, text.Content)
			}
			return TurnContinue, nil
		})
	}()
	<-handle.settled
	select {
	case <-done:
		t.Fatal("returned before settled output arrived")
	default:
	}
	events <- runtime.SessionEvent{Epoch: "owner", Sequence: 1, TurnID: "original", Event: runtime.AgentChoice("root", "s", "initial")}
	events <- runtime.SessionEvent{Epoch: "owner", Sequence: 2, TurnID: "original", Event: runtime.StreamStopped("s", "root", "normal")}
	events <- runtime.SessionEvent{Epoch: "owner", Sequence: 3, TurnID: "report:child", Event: runtime.AgentChoice("root", "s", "final follow-up")}
	events <- runtime.SessionEvent{Epoch: "owner", Sequence: 4, TurnID: "report:child", Event: runtime.StreamStopped("s", "root", "normal")}
	select {
	case result := <-done:
		require.NoError(t, result.Err)
		require.True(t, result.Stopped)
	case <-time.After(time.Second):
		t.Fatal("did not release after settlement cursor")
	}
	require.Equal(t, []string{"initial", "final follow-up"}, rendered)
}
