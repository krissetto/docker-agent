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

func TestRetainedCleanupSurvivesCloseAndRunsOnceAtShutdown(t *testing.T) {
	t.Parallel()
	s := New(nil)
	var retained, ordinary atomic.Int32
	_, err := s.AddSession(t.Context(), nil, session.New(session.WithID("retained")), "", func() { retained.Add(1) })
	require.NoError(t, err)
	_, err = s.AddSession(t.Context(), nil, session.New(session.WithID("ordinary")), "", func() { ordinary.Add(1) })
	require.NoError(t, err)
	s.RetainCleanupUntilShutdown("retained")
	s.RetainCleanupUntilShutdown("retained")
	s.RetainCleanupUntilShutdown("missing")
	s.CloseSession("retained")
	s.CloseSession("retained")
	s.CloseSession("ordinary")
	require.Eventually(t, func() bool { return ordinary.Load() == 1 }, time.Second, time.Millisecond)
	require.Zero(t, retained.Load())
	s.Shutdown()
	s.Shutdown()
	require.Equal(t, int32(1), retained.Load())
	require.Equal(t, int32(1), ordinary.Load())
}

func TestRetainedCleanupDoesNotReplaceNewOwnerCleanup(t *testing.T) {
	t.Parallel()
	s := New(nil)
	var old, current atomic.Int32
	sess := session.New(session.WithID("owner"))
	_, err := s.AddSession(t.Context(), nil, sess, "", func() { old.Add(1) })
	require.NoError(t, err)
	s.RetainCleanupUntilShutdown(sess.ID)
	s.ReplaceRunnerApp(t.Context(), sess.ID, SpawnedSession{Session: sess, Ownership: RuntimeOwned, Cleanup: func() { current.Add(1) }}, "")
	s.CloseSession(sess.ID)
	require.Eventually(t, func() bool { return current.Load() == 1 }, time.Second, time.Millisecond)
	require.Zero(t, old.Load())
	s.Shutdown()
	require.Equal(t, int32(1), old.Load())
	require.Equal(t, int32(1), current.Load())
}

func TestRetainedCleanupRunsAfterObserverDetachmentOutsideLock(t *testing.T) {
	t.Parallel()
	s := New(nil)
	var calls atomic.Int32
	_, err := s.AddSession(t.Context(), nil, session.New(session.WithID("owner")), "", func() {
		assert.Zero(t, s.Count(), "cleanup runs only after supervisor has detached active runners")
		calls.Add(1)
	})
	require.NoError(t, err)
	s.RetainCleanupUntilShutdown("owner")
	_, err = s.AddSession(t.Context(), nil, session.New(session.WithID("viewer")), "", nil)
	require.NoError(t, err)
	s.CloseSession("owner")
	s.Shutdown()
	require.Equal(t, int32(1), calls.Load(), "callback can reenter supervisor without its mutex held")
}
