package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type runReceiptHandle struct {
	projectionSession
	err error
}

func (h *runReceiptHandle) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{SessionID: h.id, TurnID: "accepted"}, h.err
}

func TestRunWithSubmissionReceiptAndErrorOwnership(t *testing.T) {
	for _, failed := range []bool{false, true} {
		sess := session.New()
		h := &runReceiptHandle{projectionSession: projectionSession{id: sess.ID}}
		if failed {
			h.err = errors.New("admission rejected")
		}
		a := New(t.Context(), &reviewSubmitRuntime{handle: h}, sess, runtime.SessionBinding{})
		receipt, err := a.RunWithSubmission(t.Context(), func() {}, "prompt", nil)
		if failed {
			require.EqualError(t, err, "admission rejected")
			require.Empty(t, receipt.TurnID)
		} else {
			require.NoError(t, err)
			require.Equal(t, "accepted", receipt.TurnID)
		}
		require.Empty(t, a.events, "receipt API leaves all error reporting to its caller")
		a.Run(t.Context(), func() {}, "prompt", nil)
		if failed {
			require.Len(t, a.events, 1, "compatibility Run emits the error exactly once")
			require.Equal(t, "admission rejected", (<-a.events).(*runtime.ErrorEvent).Error)
		} else {
			require.Empty(t, a.events)
		}
	}
}
