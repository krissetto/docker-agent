package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An explicit root stop must stay observable as Stopped after its durable
// withdrawal, whichever of the owner's registry publish or stop apply consumes
// the one-shot withdraw intent first.
func TestExplicitRootStopStaysObservableAfterWithdrawal(t *testing.T) {
	t.Run("publish consumes intent before apply", func(t *testing.T) {
		rt, sess := newSessionFixture(t)
		handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
		require.NoError(t, err)
		h := handle.(*sessionHandle)
		d := h.driver

		// The owner is parked on d.mu, so apply cannot run before this publish.
		d.mu.Lock()
		d.requestStopWithIntent(false, true)
		d.publishRegistryStateLocked()
		d.mu.Unlock()
		require.NoError(t, d.awaitStop(t.Context()))
		require.NoError(t, d.withdrawStoppedInputs(t.Context()))

		status, err := h.Status(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Stopped, "explicit root stop lost its observable Stopped after withdrawal")
	})

	t.Run("apply consumes intent before publish", func(t *testing.T) {
		rt, sess := newSessionFixture(t)
		handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
		require.NoError(t, err)
		h := handle.(*sessionHandle)
		d := h.driver

		d.requestStopWithIntent(false, true)
		require.NoError(t, d.awaitStop(t.Context()))
		d.mu.Lock()
		d.publishRegistryStateLocked()
		d.mu.Unlock()
		require.NoError(t, d.withdrawStoppedInputs(t.Context()))

		status, err := h.Status(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Stopped)
	})
}
