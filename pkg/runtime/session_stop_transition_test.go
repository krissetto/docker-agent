package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type stopFaultStore struct {
	session.CoordinationStore
	session.ChildCommitBatchStore

	fail      bool
	ambiguous bool
	hideOnce  bool
}

func (s *stopFaultStore) CommitChildren(ctx context.Context, commits []session.ChildCommit) error {
	if s.fail {
		s.fail = false
		return errors.New("transient stop failure")
	}
	err := s.ChildCommitBatchStore.CommitChildren(ctx, commits)
	if err == nil && s.ambiguous {
		s.ambiguous = false
		s.hideOnce = true
		return context.DeadlineExceeded
	}
	return err
}

func (s *stopFaultStore) LoadChildren(ctx context.Context, root string) ([]session.ChildRecord, error) {
	if s.hideOnce {
		s.hideOnce = false
		return nil, context.DeadlineExceeded
	}
	return s.CoordinationStore.LoadChildren(ctx, root)
}

func TestSessionStopSubtreeCommitFailureRetryAndRestore(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			store := coordinationSQLite(t)
			rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
			root := coordinationCreate(t, owner.Runtime(), "root", "")
			child := coordinationCreate(t, owner.Runtime(), "child", root.ID())
			// The canonical child record supplies the stable stop identity.
			id, ok := rt.subagents.nodeForSession(child.ID())
			require.True(t, ok)
			coord := store.(session.CoordinationStore)
			original, err := coord.LoadChildren(t.Context(), root.ID())
			require.NoError(t, err)
			require.Len(t, original, 1)
			fault := &stopFaultStore{CoordinationStore: coord, ChildCommitBatchStore: store.(session.ChildCommitBatchStore), fail: !ambiguous, ambiguous: ambiguous}
			rt.subagents.persistMu.Lock()
			rt.subagents.coord = fault
			rt.subagents.persistMu.Unlock()
			_, err = rt.subagents.stopChild(root.ID(), id)
			require.Error(t, err)
			live, _ := rt.subagents.Read(id)
			assert.NotEqual(t, subagent.NodeStopped, live.state, "failed acknowledgement is not published as a stop")
			assert.False(t, child.(*sessionHandle).driver.isStopped(), "no destructive cancellation before commit acknowledgement")
			_, err = rt.subagents.stopChild(root.ID(), id)
			require.NoError(t, err)
			_, err = rt.subagents.stopChild(root.ID(), id)
			require.NoError(t, err, "idempotent")
			records, err := coord.LoadChildren(t.Context(), root.ID())
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Equal(t, subagent.NodeStopped, records[0].Node.State)
			assert.Equal(t, original[0].Revision+1, records[0].Revision)
			// Durable records win over stale running topology after restart.
			require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context())))
			restored, owner2 := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
			rootSession, err := store.GetSession(t.Context(), root.ID())
			require.NoError(t, err)
			stale := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: subagent.SessionRootID(root.ID()), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(root.ID()), Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: original[0].Node}}}}}
			_, err = restored.subagents.Restore(t.Context(), rootSession, stale)
			require.NoError(t, err)
			loaded, ok := restored.subagents.Read(id)
			require.True(t, ok)
			assert.Equal(t, subagent.NodeStopped, loaded.state)
			_, err = owner2.Runtime().SessionByID(child.ID())
			require.Error(t, err, "terminal child has no executable driver")
		})
	}
}

func TestSessionStopRacesSettlementWithoutRevival(t *testing.T) {
	store := coordinationSQLite(t)
	rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	for i := range 20 {
		child := coordinationCreate(t, owner.Runtime(), string(rune('a'+i)), root.ID())
		id, ok := rt.subagents.nodeForSession(child.ID())
		require.True(t, ok)
		d := child.(*sessionHandle).driver
		d.mu.Lock()
		d.generationResult = "result"
		d.mu.Unlock()
		var wg sync.WaitGroup
		wg.Go(func() { _, err := rt.subagents.stopChild(root.ID(), id); assert.NoError(t, err) })
		wg.Go(func() { assert.NoError(t, rt.subagents.completeSessionTurn(d, "turn", "")) })
		wg.Wait()
		read, ok := rt.subagents.Read(id)
		require.True(t, ok)
		assert.Equal(t, subagent.NodeStopped, read.state)
		records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), root.ID())
		require.NoError(t, err)
		for _, record := range records {
			if record.Node.ID == id {
				assert.Equal(t, subagent.NodeStopped, record.Node.State)
			}
		}
	}
}
