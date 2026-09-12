package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func testTree(sessionID string) subagent.Snapshot {
	root := subagent.SessionRootID(sessionID)
	return subagent.Snapshot{
		Root: root,
		Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: root, Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "a1b2c", Agent: "coder", Parent: root, State: subagent.NodeCompleted}},
			},
		}},
	}
}

func TestSubagentTreeAccessorsCopySnapshots(t *testing.T) {
	s := New()
	snapshot := testTree(s.ID)
	s.SetSubagentTree(&snapshot)

	snapshot.Nodes[0].Children[0].Node.State = subagent.NodeFailed
	stored := s.GetSubagentTree()
	require.NotNil(t, stored)
	assert.Equal(t, subagent.NodeCompleted, stored.Nodes[0].Children[0].Node.State)

	stored.Nodes[0].Children[0].Node.State = subagent.NodeStopped
	again := s.GetSubagentTree()
	require.NotNil(t, again)
	assert.Equal(t, subagent.NodeCompleted, again.Nodes[0].Children[0].Node.State)
}

func TestSQLiteSubagentStoreRoundTrip(t *testing.T) {
	t.Parallel()

	tempDB := filepath.Join(t.TempDir(), "test_store.db")
	store, err := newSQLiteStoreForTest(t, tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()
	trees := store.(*SQLiteSessionStore)

	require.NoError(t, store.AddSession(t.Context(), &Session{ID: "sess-1", CreatedAt: time.Now()}))

	// Missing tree loads as nil, no error.
	got, err := trees.LoadTree(t.Context(), "sess-1")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Save + load round-trip.
	snap := testTree("sess-1")
	require.NoError(t, trees.SaveTree(t.Context(), "sess-1", snap))
	got, err = trees.LoadTree(t.Context(), "sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Nodes, 1)
	require.Len(t, got.Nodes[0].Children, 1)
	assert.Equal(t, "coder", got.Nodes[0].Children[0].Node.Agent)
	assert.Equal(t, subagent.NodeCompleted, got.Nodes[0].Children[0].Node.State)

	// SaveTree upserts the latest snapshot.
	snap.Nodes[0].Children[0].Node.State = subagent.NodeFailed
	require.NoError(t, trees.SaveTree(t.Context(), "sess-1", snap))
	got, err = trees.LoadTree(t.Context(), "sess-1")
	require.NoError(t, err)
	assert.Equal(t, subagent.NodeFailed, got.Nodes[0].Children[0].Node.State)

	// Deleting the session cascades to its tree row.
	require.NoError(t, store.DeleteSession(t.Context(), "sess-1"))
	got, err = trees.LoadTree(t.Context(), "sess-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSQLiteSubagentStoreRequiresExistingSession(t *testing.T) {
	t.Parallel()

	tempDB := filepath.Join(t.TempDir(), "test_store.db")
	store, err := newSQLiteStoreForTest(t, tempDB)
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()
	trees := store.(*SQLiteSessionStore)

	// The foreign key keeps orphan tree rows out of the table.
	require.Error(t, trees.SaveTree(t.Context(), "no-such-session", testTree("no-such-session")))
	require.Error(t, trees.SaveTree(t.Context(), "", testTree("x")))
}
