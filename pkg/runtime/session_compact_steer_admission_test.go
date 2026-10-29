package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// Off the dormant view only user steering blocks idle compaction; agent guidance
// never does, and dormant steering compacts until explicit admission clears it.
func TestCompactAdmissionDormantSteerPolicy(t *testing.T) {
	newDriver := func(dormant bool, origin session.InputOrigin) *sessionDriver {
		d := newSessionDriver(&LocalRuntime{maxPendingMailbox: defaultMaxSubagentMailbox}, &session.Session{ID: "session-id"})
		d.viewDormant = dormant
		d.pending = []QueuedMessage{{Content: "guide", RequestID: "steer", InputOrigin: origin, InputMode: "steer"}}
		return d
	}
	compact := func(d *sessionDriver) *SessionError {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.admitLocked(SessionOperationCompact)
	}
	t.Run("idle agent steer compacts", func(t *testing.T) {
		assert.Nil(t, compact(newDriver(false, session.InputOriginAgent)))
	})
	t.Run("idle user steer rejects", func(t *testing.T) {
		err := compact(newDriver(false, session.InputOriginUser))
		require.NotNil(t, err)
		assert.Equal(t, SessionOperationCompactPending, err.Operation)
		assert.Equal(t, SessionErrorCapacity, err.Kind)
	})
	t.Run("dormant user steer compacts", func(t *testing.T) {
		d := newDriver(true, session.InputOriginUser)
		assert.Nil(t, compact(d))
		require.Len(t, d.pending, 1)
		assert.Equal(t, "steer", d.pending[0].RequestID)
	})
	t.Run("explicit admission clears dormancy and then rejects user steer", func(t *testing.T) {
		d := newDriver(true, session.InputOriginUser)
		d.mu.Lock()
		cleared := d.authorizeViewLocked()
		d.mu.Unlock()
		require.True(t, cleared)
		err := compact(d)
		require.NotNil(t, err)
		assert.Equal(t, SessionOperationCompactPending, err.Operation)
	})
}
