package acp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

type elicitationHandle struct {
	fakeRuntime

	events        chan runtime.SessionEvent
	once          sync.Once
	responses     []runtime.InteractionResponse
	responseErr   error
	cancelledTurn string
}

func (h *elicitationHandle) Observe(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
	h.events = make(chan runtime.SessionEvent, 2)
	h.events <- runtime.SessionEvent{SessionID: testSessionID, TurnID: "request-1", InteractionID: "interaction-1", Event: &runtime.ElicitationRequestEvent{SessionID: testSessionID, ElicitationID: "elicitation-1", RequestID: "interaction-1", Message: "enter value"}}
	return runtime.Observation{Events: h.events, Cancel: func() {}}, nil
}

func (h *elicitationHandle) Respond(_ context.Context, response runtime.InteractionResponse) error {
	h.responses = append(h.responses, response)
	if h.responseErr == nil {
		h.finish()
	}
	return h.responseErr
}

func (h *elicitationHandle) Cancel(_ context.Context, turnID string) (runtime.CancelResult, error) {
	h.cancelledTurn = turnID
	h.finish()
	return runtime.CancelResult{Outcome: runtime.CancelAccepted}, nil
}

func (h *elicitationHandle) finish() {
	h.once.Do(func() {
		h.events <- runtime.SessionEvent{SessionID: testSessionID, TurnID: "request-1", Event: &runtime.StreamStoppedEvent{}}
	})
}

func TestACPDeclinesUnsupportedElicitation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "decline", true: "response failure"}[fail], func(t *testing.T) {
			h := &elicitationHandle{}
			if fail {
				h.responseErr = errors.New("response failed")
			}
			fixture := newRunAgentFixture(t, &h.fakeRuntime, &captureWriter{})
			fixture.sess.session = h
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := fixture.agent.runAgent(ctx, fixture.sess, runtime.TurnInput{Content: "hello"})
			if fail {
				require.ErrorIs(t, err, h.responseErr)
				require.Equal(t, "request-1", h.cancelledTurn)
			} else {
				require.NoError(t, err)
				require.Empty(t, h.cancelledTurn)
			}
			require.Equal(t, []runtime.InteractionResponse{{InteractionID: "interaction-1", Kind: runtime.InteractionElicitation, ElicitationID: "elicitation-1", Elicitation: runtime.ElicitationResult{Action: tools.ElicitationActionDecline}}}, h.responses)
			require.Zero(t, h.stopWakes())
		})
	}
}
