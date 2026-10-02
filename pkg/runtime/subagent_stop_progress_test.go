package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type blockedSubtreeStopStore struct {
	session.CoordinationStore
	session.ChildCommitBatchStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockedSubtreeStopStore) CommitChildren(ctx context.Context, commits []session.ChildCommit) error {
	close(s.entered)
	select {
	case <-s.release:
		return s.ChildCommitBatchStore.CommitChildren(ctx, commits)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSubagentStopPersistenceDoesNotBlockIndependentRootAdmission(t *testing.T) {
	store := coordinationSQLite(t)
	rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", root.ID())
	independent := coordinationCreate(t, owner.Runtime(), "independent", "")
	id, ok := rt.subagents.nodeForSession(child.ID())
	require.True(t, ok)
	fault := &blockedSubtreeStopStore{CoordinationStore: store.(session.CoordinationStore), ChildCommitBatchStore: store.(session.ChildCommitBatchStore), entered: make(chan struct{}), release: make(chan struct{})}
	rt.subagents.persistMu.Lock()
	rt.subagents.coord = fault
	rt.subagents.persistMu.Unlock()
	var release sync.Once
	defer release.Do(func() { close(fault.release) })
	stopped := make(chan error, 1)
	go func() { _, err := rt.subagents.stopChild(root.ID(), id); stopped <- err }()
	coordinationWait(t, fault.entered)

	// Test admission itself, not root completion's manager-owned projections.
	// A root uses no descendant slot, while stopped child admission remains gated.
	driver := independent.(*sessionHandle).driver
	type result struct {
		generation uint64
		err        error
	}
	started := make(chan result, 1)
	go func() {
		_, generation, _, err := driver.prepareStart(t.Context(), false)
		started <- result{generation, err}
	}()
	var outcome result
	select {
	case outcome = <-started:
	case <-time.After(time.Second):
		release.Do(func() { close(fault.release) })
		outcome = <-started
		t.Error("independent root admission waited for subtree stop persistence")
	}
	require.NoError(t, outcome.err)
	release.Do(func() { close(fault.release) })
	require.NoError(t, <-stopped)
	require.True(t, child.(*sessionHandle).driver.isStopped())
	node, ok := rt.subagents.tree.Node(id)
	require.True(t, ok)
	require.Equal(t, subagent.NodeStopped, node.State)
	require.Error(t, rt.subagents.admitChildRun(id))
	driver.StopAll()
	driver.finishRun(outcome.generation, "")
	driver.wg.Done()
}
