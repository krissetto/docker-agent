package subagent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTreeAddRejectsMissingParentWithoutMutation(t *testing.T) {
	tree := NewTree()
	require.NoError(t, tree.Add(Node{ID: "root", Agent: "root"}))
	before := tree.Snapshot()

	err := tree.Add(Node{ID: "orphan", Agent: "worker", Parent: "missing"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNodeNotFound)
	assert.Equal(t, before, tree.Snapshot())
	_, exists := tree.Node("orphan")
	assert.False(t, exists)
}

func TestTreeAddSubtreeRejectsInvalidBatchWithoutMutationOrNotification(t *testing.T) {
	tree := NewTree()
	require.NoError(t, tree.Add(Node{ID: "existing", Agent: "root"}))
	before := tree.Snapshot()
	updates, cancel := tree.Subscribe(2)
	defer cancel()
	<-updates

	for name, nodes := range map[string][]Node{
		"duplicate existing": {{ID: "existing", Agent: "root"}},
		"duplicate batch":    {{ID: "new", Agent: "root"}, {ID: "new", Agent: "root"}},
		"missing parent":     {{ID: "child", Agent: "worker", Parent: "missing"}},
		"self cycle":         {{ID: "self", Agent: "root", Parent: "self"}},
		"multi node cycle":   {{ID: "a", Agent: "root", Parent: "b"}, {ID: "b", Agent: "root", Parent: "a"}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, tree.AddSubtree(nodes))
			assert.Equal(t, before, tree.Snapshot())
			select {
			case got := <-updates:
				t.Fatalf("invalid addition notified observers: %+v", got)
			case <-time.After(10 * time.Millisecond):
			}
		})
	}
}

func TestTreeAddSubtreePreservesExistingNodesAndPublishesOnce(t *testing.T) {
	tree := NewTree()
	tree.now = func() time.Time { return time.Unix(1, 0) }
	require.NoError(t, tree.Add(Node{ID: "existing", Agent: "root"}))
	before, ok := tree.Node("existing")
	require.True(t, ok)

	tree.now = func() time.Time { return time.Unix(2, 0) }
	updates, cancel := tree.Subscribe(3)
	defer cancel()
	<-updates
	require.NoError(t, tree.AddSubtree([]Node{
		{ID: "new-root", Agent: "root"},
		{ID: "child", Agent: "worker", Parent: "new-root"},
	}))

	after, ok := tree.Node("existing")
	require.True(t, ok)
	assert.Equal(t, before, after, "adding a subtree must not rewrite unrelated nodes")
	got := <-updates
	require.Len(t, got.Nodes, 2)
	assert.Equal(t, NodeID("new-root"), got.Nodes[1].Node.ID)
	require.Len(t, got.Nodes[1].Children, 1)
	select {
	case extra := <-updates:
		t.Fatalf("subtree addition published more than once: %+v", extra)
	case <-time.After(10 * time.Millisecond):
	}
}

func TestNewIDIsFiveHexChars(t *testing.T) {
	id := string(NewID())
	assert.Len(t, id, 5)
	assert.Regexp(t, `^[0-9a-f]{5}$`, id)
}

func TestTreeSnapshotIncludesEveryRoot(t *testing.T) {
	tr := NewTree()
	require.NoError(t, tr.Add(Node{ID: "root:a", Agent: "a"}))
	require.NoError(t, tr.Add(Node{ID: "root:b", Agent: "b"}))
	require.NoError(t, tr.Add(Node{ID: "child", Agent: "worker", Parent: "root:b"}))

	snapshot := tr.Snapshot()
	assert.Empty(t, snapshot.Root, "multi-root snapshots have no single root")
	require.Len(t, snapshot.Nodes, 2)
	assert.Equal(t, NodeID("root:a"), snapshot.Nodes[0].Node.ID)
	assert.Equal(t, NodeID("root:b"), snapshot.Nodes[1].Node.ID)
	require.Len(t, snapshot.Nodes[1].Children, 1)
	assert.Equal(t, NodeID("child"), snapshot.Nodes[1].Children[0].Node.ID)
}

func TestTreeCancelConcurrentWithUpdateDoesNotPanic(t *testing.T) {
	tr := NewTree()
	require.NoError(t, tr.Add(Node{ID: "root", Agent: "root"}))

	for range 1_000 {
		_, cancel := tr.Subscribe(1)
		updated := make(chan struct{})
		go func() {
			_ = tr.Update("root", func(n *Node) { n.State = NodeRunning })
			close(updated)
		}()
		cancel()
		<-updated
	}
}

func TestTreeNewNodeIDAvoidsExistingIDs(t *testing.T) {
	tr := NewTree()
	id := tr.NewNodeID()
	err := tr.Add(Node{ID: id, Agent: "a"})
	require.NoError(t, err)

	for range 100 {
		next := tr.NewNodeID()
		assert.NotEqual(t, id, next)
		assert.Len(t, string(next), 5)
	}
}
