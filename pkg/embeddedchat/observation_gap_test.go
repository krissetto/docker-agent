package embeddedchat

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

type gapObservationHandle struct {
	*fakeRuntime

	observes atomic.Int32
}

func (h *gapObservationHandle) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	n := h.observes.Add(1)
	out := make(chan runtime.SessionEvent, 1)
	if n < 5 {
		out <- runtime.SessionEvent{Gap: true}
	} else {
		go func() { <-ctx.Done(); close(out) }()
	}
	return runtime.Observation{Initial: []runtime.SessionSnapshot{{Epoch: "epoch", Status: runtime.SessionStatus{SessionID: h.ID()}, Cursor: uint64(n)}}, Events: out, Cancel: func() {}}, nil
}

func TestObserveRepeatedGapsRebaselineAndFailExplicitly(t *testing.T) {
	h := &gapObservationHandle{fakeRuntime: newFakeRuntime()}
	s := &Session{handle: h}
	out, err := s.Observe(t.Context())
	require.NoError(t, err)
	baselines := 0
	var failure error
	for event := range out {
		if event.Snapshot != nil {
			baselines++
		}
		if event.Err != nil {
			failure = event.Err
		}
	}
	require.Equal(t, 4, baselines)
	require.ErrorContains(t, failure, "repeated gaps")
	require.Equal(t, int32(4), h.observes.Load())
	require.NoError(t, s.Close())
}
