package supervisor

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestReplaceRunnerAppOwnershipLifecycle(t *testing.T) {
	t.Parallel()

	s := New(nil)
	var first, second atomic.Int32
	original := session.New(session.WithID("tab"))
	_, err := s.AddSession(t.Context(), nil, original, "old", func() { first.Add(1) })
	require.NoError(t, err)

	// Borrowed replacements retain the sole existing owner cleanup even when
	// repeated rapidly; cancellation only detaches the old view subscription.
	for range 20 {
		s.ReplaceRunnerApp(t.Context(), original.ID, SpawnedSession{
			Session:   original,
			Ownership: RuntimeBorrowed,
		}, "borrowed")
	}
	assert.Zero(t, first.Load())

	// An explicitly owned replacement retires the old owner and installs its
	// own exactly-once final cleanup.
	s.ReplaceRunnerApp(t.Context(), original.ID, SpawnedSession{
		Session:   original,
		Ownership: RuntimeOwned,
		Cleanup:   func() { second.Add(1) },
	}, "owned")
	require.Eventually(t, func() bool { return first.Load() == 1 }, time.Second, time.Millisecond)
	assert.Zero(t, second.Load())

	s.Shutdown()
	assert.Equal(t, int32(1), first.Load())
	assert.Equal(t, int32(1), second.Load())
	s.Shutdown()
	assert.Equal(t, int32(1), first.Load())
	assert.Equal(t, int32(1), second.Load())
}
