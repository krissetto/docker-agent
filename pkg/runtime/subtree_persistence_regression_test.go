package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSubtreeStopUncertainAcknowledgementKeepsQuarantineUntilRetry(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			rt, store, root := newRestoreFixture(t)
			planner, err := rt.team.Agent("planner")
			require.NoError(t, err)
			child := session.New(session.WithID("quarantined-child"), session.WithAgentName("planner"), session.WithParentID(root.ID))
			child.AsyncSubagent = true
			require.NoError(t, rt.subagents.registerIdleChild(root, "root", child, planner, subagent.AllowedSubagent{Agent: "planner"}))
			grand := session.New(session.WithID("quarantined-grand"), session.WithAgentName("planner"), session.WithParentID(child.ID))
			grand.AsyncSubagent = true
			require.NoError(t, rt.subagents.registerIdleChild(child, "planner", grand, planner, subagent.AllowedSubagent{Agent: "planner"}))
			id, ok := rt.SubagentNodeForSession(child.ID)
			require.True(t, ok)
			grandID, ok := rt.SubagentNodeForSession(grand.ID)
			require.True(t, ok)
			coord := store.(session.CoordinationStore)
			original, err := coord.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			fault := &stopFaultStore{CoordinationStore: coord, ChildCommitBatchStore: store.(session.ChildCommitBatchStore), fail: !ambiguous, ambiguous: ambiguous}
			rt.subagents.persistMu.Lock()
			rt.subagents.coord = fault
			rt.subagents.persistMu.Unlock()
			driver, ok := rt.sessionDrivers.Lookup(child.ID)
			require.True(t, ok)
			handle := &sessionHandle{runtime: rt, driver: driver, sessionID: child.ID}
			require.Error(t, handle.StopSubtree(t.Context()))
			for _, target := range []struct {
				sess *session.Session
				node subagent.NodeID
			}{{child, id}, {grand, grandID}} {
				require.Error(t, rt.subagents.sessionAdmissionError(target.sess.ID))
				require.Error(t, rt.subagents.admitChildRun(target.node))
				require.Error(t, rt.subagents.ensureChildDriver(t.Context(), target.node))
				_, err = rt.subagents.Spawn(target.sess, "planner", subagent.AllowedSubagent{Agent: "planner"}, "rejected")
				require.Error(t, err)
				d, found := rt.sessionDrivers.Lookup(target.sess.ID)
				require.True(t, found)
				assert.False(t, d.isStopped(), "no destructive cancellation before acknowledged commit")
				before := d.session().MessagesSnapshot()
				inputHandle := &sessionHandle{runtime: rt, driver: d, sessionID: target.sess.ID}
				_, err = inputHandle.Submit(t.Context(), TurnInput{Content: "rejected turn", RequestID: "quarantined-submit"})
				require.ErrorIs(t, err, &SessionError{Kind: SessionErrorStopped})
				_, err = inputHandle.Steer(t.Context(), TurnInput{Content: "rejected steering", RequestID: "quarantined-steer"})
				require.ErrorIs(t, err, &SessionError{Kind: SessionErrorStopped})
				assert.Equal(t, before, d.session().MessagesSnapshot(), "quarantine rejects input without acceptance or transcript mutation")
				require.ErrorIs(t, rt.subagents.completeSessionTurn(d, "quarantined-turn", ""), &SessionError{Kind: SessionErrorPersistence})
			}
			receipt, err := rt.subagents.sendCommunicationToChild(t.Context(), root.ID, id, "rejected", "quarantined-request", subagent.DeliveryGuidance)
			require.Error(t, err)
			assert.False(t, receipt.Accepted)
			grandDriver, found := rt.sessionDrivers.Lookup(grand.ID)
			require.True(t, found)
			grandHandle := &sessionHandle{runtime: rt, driver: grandDriver, sessionID: grand.ID}
			require.Error(t, grandHandle.StopSubtree(t.Context()), "overlapping stops cannot supersede the original batch")
			require.NoError(t, handle.StopSubtree(t.Context()))
			require.NoError(t, handle.StopSubtree(t.Context()))
			rows, err := coord.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, rows, 2)
			for _, row := range rows {
				assert.Equal(t, subagent.NodeStopped, row.Node.State)
				for _, before := range original {
					if before.Node.ID == row.Node.ID {
						assert.Equal(t, before.Revision+1, row.Revision)
					}
				}
				require.NoError(t, rt.subagents.sessionAdmissionError(row.Node.SessionID), "authoritative retry releases quarantine")
				d, found := rt.sessionDrivers.Lookup(row.Node.SessionID)
				require.True(t, found)
				assert.True(t, d.isStopped())
				require.NoError(t, rt.subagents.completeSessionTurn(d, "quarantined-turn", ""), "terminal settlement can finish after recovery")
			}
		})
	}
}

func TestRestoreSessionTreeKeepsSyntheticRootOnFailureAndPublication(t *testing.T) {
	for _, failure := range []string{"collision", "preparation", "none"} {
		t.Run(failure, func(t *testing.T) {
			rt, store, root := newRestoreFixture(t)
			root.AgentName = "root"
			_, err := rt.CreateSession(t.Context(), root, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			rt.subagents.ensureRoot(root, "root")
			tracked := rt.subagents.sessions[root.ID]
			require.NotNil(t, tracked.unwatch)
			before := rt.SubagentTree().Snapshot()
			updates, cancel := rt.SubagentTree().Subscribe(10)
			defer cancel()
			<-updates
			child := session.New(session.WithID("restore-child"), session.WithAgentName("planner"), session.WithParentID(root.ID))
			require.NoError(t, store.AddSession(t.Context(), child))
			if failure == "collision" {
				rt.sessionDrivers.Get(child)
			}
			if failure == "preparation" {
				rt.sessionDrivers.mu.Lock()
				rt.sessionDrivers.deleted[child.ID] = struct{}{}
				rt.sessionDrivers.mu.Unlock()
			}
			rootID := subagent.SessionRootID(root.ID)
			snap := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "restore-node", Parent: rootID, SessionID: child.ID, Agent: "planner", State: subagent.NodeIdle}}}}}}
			require.NoError(t, rt.subagentStore.SaveTree(t.Context(), root.ID, snap))
			err = (&localSessionRuntimeView{runtime: rt}).RestoreSessionTree(t.Context(), root)
			assert.Same(t, tracked, rt.subagents.sessions[root.ID])
			require.NotNil(t, tracked.unwatch)
			if failure != "none" {
				require.Error(t, err)
				assert.Equal(t, before, rt.SubagentTree().Snapshot())
				select {
				case update := <-updates:
					t.Fatalf("failed restore published topology: %+v", update)
				default:
				}
			} else {
				require.NoError(t, err)
				update := <-updates
				require.Len(t, update.Nodes, 1)
				assert.Equal(t, before.Nodes[0].Node, update.Nodes[0].Node, "reuse synthetic root without remove/add publication")
				require.Len(t, update.Nodes[0].Children, 1)
			}
		})
	}
}

type budgetDeleteFaultStore struct {
	session.Store

	failID string
}

func (s *budgetDeleteFaultStore) DeleteSession(ctx context.Context, id string) error {
	if id == s.failID {
		return errors.New("delete failure")
	}
	return s.Store.DeleteSession(ctx, id)
}

func TestPublicDeleteSessionReleasesBudgetOnlyAfterSuccessfulDeletion(t *testing.T) {
	for _, failure := range []string{"root", "child", "none"} {
		t.Run(failure, func(t *testing.T) {
			store := coordinationSQLite(t)
			rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("worker"))
			root := coordinationCreate(t, owner.Runtime(), "budget-root", "")
			child := coordinationCreate(t, owner.Runtime(), "budget-child", root.ID())
			rt.budgetMu.Lock()
			rt.rootBudgets = map[string]*budgetSet{root.ID(): {}}
			rt.budgetMu.Unlock()
			fault := &budgetDeleteFaultStore{Store: store}
			if failure == "root" {
				fault.failID = root.ID()
			}
			if failure == "child" {
				fault.failID = child.ID()
			}
			rt.sessionStore = fault
			err := owner.Runtime().DeleteSession(t.Context(), root.ID())
			if failure != "none" {
				require.Error(t, err)
				rt.budgetMu.Lock()
				_, retained := rt.rootBudgets[root.ID()]
				rt.budgetMu.Unlock()
				assert.True(t, retained, "failed deletion retains wallet")
				fault.failID = ""
				require.NoError(t, owner.Runtime().DeleteSession(t.Context(), root.ID()))
			} else {
				require.NoError(t, err)
			}
			rt.budgetMu.Lock()
			_, retained := rt.rootBudgets[root.ID()]
			rt.budgetMu.Unlock()
			assert.False(t, retained, "successful persisted deletion releases wallet")
			_, err = store.GetSession(t.Context(), root.ID())
			require.ErrorIs(t, err, session.ErrNotFound)
		})
	}
}
