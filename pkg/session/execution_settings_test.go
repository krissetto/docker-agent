package session

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteExecutionSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithAgentName("active-agent"), WithAsyncSubagent(true), WithNonInteractive(true),
		WithAttachedFiles([]string{"/tmp/input.txt"}), WithDelegationLineage([]string{"root", "worker"}),
		WithMaxConsecutiveToolCalls(8), WithMaxOldToolCallTokens(100), WithMaxToolResultTokens(200),
		WithAttributes(map[string]string{"binding.agent": "creation-agent"}))
	sess.SetSafetyPolicy(SafetyPolicyStrict)
	sess.ToggleYolo()
	require.NoError(t, store.AddSession(t.Context(), sess))
	check := func() {
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		assert.Equal(t, SafetyPolicyAutonomous, loaded.GetSafetyPolicy())
		assert.Equal(t, SafetyPolicyStrict, loaded.GetPriorSafetyPolicy())
		loaded.ToggleYolo()
		assert.Equal(t, SafetyPolicyStrict, loaded.GetSafetyPolicy())
		assert.Equal(t, sess.AgentName, loaded.AgentName)
		assert.True(t, loaded.AsyncSubagent)
		assert.True(t, loaded.NonInteractive)
		assert.Equal(t, sess.AttachedFiles, loaded.AttachedFiles)
		assert.Equal(t, sess.DelegationLineage, loaded.DelegationLineage)
		assert.Equal(t, 8, loaded.MaxConsecutiveToolCalls)
		assert.Equal(t, 100, loaded.MaxOldToolCallTokens)
		assert.Equal(t, 200, loaded.MaxToolResultTokens)
		assert.Equal(t, "creation-agent", loaded.Attributes["binding.agent"])
	}
	check()
	sess.AgentName = "handoff-agent"
	require.NoError(t, store.UpdateSession(t.Context(), sess))
	check()
	require.NoError(t, store.PersistCompaction(t.Context(), sess, 1, 2, Item{Summary: "summary"}))
	check()
}

func TestTurnOutcomesPersistAndClone(t *testing.T) {
	for _, store := range []Store{openMemoryStore(t), NewInMemorySessionStore()} {
		sess := New()
		sess.SetTurnOutcome("accepted", "completed")
		require.NoError(t, store.AddSession(t.Context(), sess))
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Equal(t, "completed", loaded.TurnOutcome("accepted"))
		cloned := loaded.Clone()
		cloned.SetTurnOutcome("accepted", "failed")
		require.Equal(t, "completed", loaded.TurnOutcome("accepted"))
		require.NoError(t, store.UpdateSession(t.Context(), cloned))
		updated, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Equal(t, "failed", updated.TurnOutcome("accepted"))
	}
}

func TestTurnOutcomeRetentionSurvivesReload(t *testing.T) {
	const completedTurns = 4096
	for _, store := range []Store{openMemoryStore(t), NewInMemorySessionStore()} {
		sess := New()
		for i := range completedTurns {
			sess.SetTurnOutcome(strconv.Itoa(i), "completed")
		}
		require.Len(t, sess.TurnOutcomesSnapshot(), completedTurns)
		require.Len(t, sess.TurnOutcomeOrderSnapshot(), completedTurns)
		require.NoError(t, store.AddSession(t.Context(), sess))
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Equal(t, sess.TurnOutcomesSnapshot(), loaded.TurnOutcomesSnapshot())
		require.Equal(t, sess.TurnOutcomeOrderSnapshot(), loaded.TurnOutcomeOrderSnapshot())
		for _, clone := range []*Session{loaded.Clone(), loaded.OwnSnapshot()} {
			require.Equal(t, loaded.TurnOutcomesSnapshot(), clone.TurnOutcomesSnapshot())
			require.Equal(t, loaded.TurnOutcomeOrderSnapshot(), clone.TurnOutcomeOrderSnapshot())
			clone.SetTurnOutcome("0", "failed")
			clone.SetTurnOutcome("new", "canceled")
			require.Equal(t, "completed", loaded.TurnOutcome("0"))
			require.Empty(t, loaded.TurnOutcome("new"))
			require.Len(t, clone.TurnOutcomeOrderSnapshot(), completedTurns+1)
			require.NoError(t, store.UpdateSession(t.Context(), clone))
			updated, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Equal(t, clone.TurnOutcomesSnapshot(), updated.TurnOutcomesSnapshot())
			require.Equal(t, clone.TurnOutcomeOrderSnapshot(), updated.TurnOutcomeOrderSnapshot())
		}
	}
}
