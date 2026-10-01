package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestChildReactivationAtomicFreshInput(t *testing.T) {
	for name, create := range map[string]func() Store{"memory": NewInMemorySessionStore, "sqlite": func() Store { return openMemoryStore(t) }} {
		t.Run(name, func(t *testing.T) {
			store := create()
			coord := store.(CoordinationStore)
			revive := store.(ChildReactivationStore)
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			admission := coordinationAdmission(root.ID)
			admission.Record.Node.State = subagent.NodeStopped
			require.NoError(t, coord.AdmitChild(t.Context(), admission))
			records, err := coord.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			record := records[0]
			input := UserMessage("fresh")
			input.TurnID, input.InputOrigin, input.InputMode = "fresh-turn", InputOriginUser, "turn"
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			_, _, err = revive.ReactivateChildWithInput(canceled, record, input)
			require.ErrorIs(t, err, context.Canceled)
			stale := record
			stale.Revision++
			_, _, err = revive.ReactivateChildWithInput(t.Context(), stale, input)
			require.ErrorIs(t, err, ErrRevisionConflict)
			wrong := record
			wrong.Node.Agent = "other"
			_, _, err = revive.ReactivateChildWithInput(t.Context(), wrong, input)
			require.Error(t, err)
			old := cloneMessage(input)
			old.TurnID = "initial-turn"
			_, _, err = revive.ReactivateChildWithInput(t.Context(), record, old)
			require.ErrorIs(t, err, ErrAlreadyExists)
			for _, origin := range []InputOrigin{InputOriginAgent, InputOriginRuntime} {
				bad := cloneMessage(input)
				bad.InputOrigin = origin
				_, _, err = revive.ReactivateChildWithInput(t.Context(), record, bad)
				require.Error(t, err)
			}
			before, err := store.GetSession(t.Context(), record.Node.SessionID)
			require.NoError(t, err)
			assert.True(t, before.MessagesSnapshot()[0].Message.Accepted)
			next, accepted, err := revive.ReactivateChildWithInput(t.Context(), record, input)
			require.NoError(t, err)
			assert.Equal(t, record.Revision+1, next.Revision)
			assert.Equal(t, subagent.NodeIdle, next.Node.State)
			assert.Equal(t, record.Node.ID, next.Node.ID)
			assert.Equal(t, input.TurnID, next.ReactivationTurnID)
			assert.True(t, accepted.Pending)
			assert.True(t, accepted.Accepted)
			_, _, err = revive.ReactivateChildWithInput(t.Context(), record, input)
			require.ErrorIs(t, err, ErrRevisionConflict)
			after, err := store.GetSession(t.Context(), record.Node.SessionID)
			require.NoError(t, err)
			items := after.MessagesSnapshot()
			require.Len(t, items, 2)
			assert.True(t, items[0].Message.Pending)
			assert.False(t, items[0].Message.Accepted)
			assert.Equal(t, "task", items[0].Message.Message.Content)
			assert.Equal(t, accepted.ID, items[1].Message.ID)
		})
	}
}

func TestSQLiteChildReactivationRollback(t *testing.T) {
	store := openMemoryStore(t)
	root := New()
	require.NoError(t, store.AddSession(t.Context(), root))
	admission := coordinationAdmission(root.ID)
	admission.Record.Node.State = subagent.NodeStopped
	require.NoError(t, store.AdmitChild(t.Context(), admission))
	records, err := store.LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(t.Context(), `CREATE TRIGGER reject_reactivation_input BEFORE INSERT ON session_items BEGIN SELECT RAISE(ABORT, 'synthetic append failure'); END`)
	require.NoError(t, err)
	input := UserMessage("fresh")
	input.TurnID, input.InputOrigin, input.InputMode = "fresh", InputOriginUser, "turn"
	_, _, err = store.ReactivateChildWithInput(t.Context(), records[0], input)
	require.Error(t, err)
	after, err := store.LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Equal(t, records, after)
	saved, err := store.GetSession(t.Context(), admission.Child.ID)
	require.NoError(t, err)
	items := saved.MessagesSnapshot()
	require.Len(t, items, 1)
	assert.True(t, items[0].Message.Pending)
	assert.True(t, items[0].Message.Accepted)
}
