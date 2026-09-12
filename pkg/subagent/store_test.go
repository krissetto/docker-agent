package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryStoreRoundTrip(t *testing.T) {
	t.Parallel()

	s := NewInMemoryStore()

	got, err := s.LoadTree(t.Context(), "sess")
	require.NoError(t, err)
	assert.Nil(t, got, "missing tree loads as nil")

	snap := Snapshot{
		Root: SessionRootID("sess"),
		Nodes: []NodeSnapshot{{
			Node: Node{ID: SessionRootID("sess"), Agent: "root", State: NodeRunning},
			Children: []NodeSnapshot{
				{Node: Node{ID: "a1b2c", Agent: "coder", State: NodeRunning}},
			},
		}},
	}
	require.NoError(t, s.SaveTree(t.Context(), "sess", snap))

	got, err = s.LoadTree(t.Context(), "sess")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, NodeRunning, got.Nodes[0].Children[0].Node.State)

	// Upsert replaces the previous snapshot.
	snap.Nodes[0].Children[0].Node.State = NodeCompleted
	require.NoError(t, s.SaveTree(t.Context(), "sess", snap))
	got, err = s.LoadTree(t.Context(), "sess")
	require.NoError(t, err)
	assert.Equal(t, NodeCompleted, got.Nodes[0].Children[0].Node.State)
}
