package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type canonicalProjectionStore struct {
	session.CoordinationStore
	records []session.ChildRecord
}

func (s *canonicalProjectionStore) LoadChildren(context.Context, string) ([]session.ChildRecord, error) {
	return s.records, nil
}

func TestCanonicalRestorePreservesAuthorityAndRejectsDisconnectedRecords(t *testing.T) {
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	rootID := subagent.SessionRootID(root.ID)
	canonical := subagent.Node{ID: "child", Parent: rootID, SessionID: "child-session", Agent: "planner", State: subagent.NodeStopped, Error: "canonical", Cost: 1}
	for _, test := range []struct {
		name      string
		records   []session.ChildRecord
		wantError bool
	}{
		{name: "matching projection", records: []session.ChildRecord{{Node: canonical}}},
		{name: "orphan", records: []session.ChildRecord{{Node: canonical}, {Node: subagent.Node{ID: "orphan", Parent: "missing"}}}, wantError: true},
		{name: "disconnected cycle", records: []session.ChildRecord{{Node: canonical}, {Node: subagent.Node{ID: "a", Parent: "b"}}, {Node: subagent.Node{ID: "b", Parent: "a"}}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &LocalRuntime{ctx: func() context.Context { return t.Context() }}
			m := newSubagentManager(r)
			defer m.Close()
			m.coord = &canonicalProjectionStore{records: test.records}
			stale := canonical
			stale.Parent, stale.State, stale.Error, stale.Cost, stale.ToolCalls = "stale-parent", subagent.NodeRunning, "stale", 2, 4
			projection := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Cost: 3}, Children: []subagent.NodeSnapshot{{Node: stale}}}}}
			got, _, err := m.canonicalSnapshot(t.Context(), root, projection)
			if test.wantError {
				require.ErrorContains(t, err, "unreachable canonical child")
				return
			}
			require.NoError(t, err)
			require.Equal(t, float64(3), got.Nodes[0].Node.Cost)
			child := got.Nodes[0].Children[0].Node
			require.Equal(t, canonical.Parent, child.Parent)
			require.Equal(t, canonical.State, child.State)
			require.Equal(t, canonical.Error, child.Error)
			require.Equal(t, float64(2), child.Cost)
			require.Equal(t, int64(4), child.ToolCalls)
			projection.Nodes[0].Children[0].Node.SessionID = "different-session"
			got, _, err = m.canonicalSnapshot(t.Context(), root, projection)
			require.NoError(t, err)
			require.Equal(t, canonical.Cost, got.Nodes[0].Children[0].Node.Cost, "metrics must match child identity")
		})
	}
}

func TestCanonicalRestoreAllowsLegacyParentOfCanonicalChild(t *testing.T) {
	r := &LocalRuntime{ctx: func() context.Context { return t.Context() }}
	m := newSubagentManager(r)
	defer m.Close()
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	rootID := subagent.SessionRootID(root.ID)
	m.coord = &canonicalProjectionStore{records: []session.ChildRecord{{Node: subagent.Node{ID: "child", Parent: "legacy-parent"}}}}
	projection := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "legacy-parent", Parent: rootID}}}}}}
	got, _, err := m.canonicalSnapshot(t.Context(), root, projection)
	require.NoError(t, err)
	require.Equal(t, subagent.NodeID("child"), got.Nodes[0].Children[0].Children[0].Node.ID)
}

func TestRestoredRootLifecycleBindsOnceAndFencesReplacedDriver(t *testing.T) {
	rt, _, root := newRestoreFixture(t)
	rootID := subagent.SessionRootID(root.ID)
	_, err := restorePersistedFixture(t, rt, root, subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeIdle}}}})
	require.NoError(t, err)
	first, err := rt.sessionDrivers.GetInitialized(t.Context(), root)
	require.NoError(t, err)
	rt.subagents.ensureRoot(root, "root")
	rt.subagents.ensureRoot(root, "root")
	first.mu.Lock()
	started, settled := first.startedCallbacksLocked(), first.settledCallbacksLocked()
	first.mu.Unlock()
	require.Len(t, started, 1)
	require.Len(t, settled, 1)
	started[0]()
	node, _ := rt.subagents.tree.Node(rootID)
	require.Equal(t, subagent.NodeRunning, node.State)
	first.mu.Lock()
	first.lastError = "failed root turn"
	first.mu.Unlock()
	settled[0]()
	node, _ = rt.subagents.tree.Node(rootID)
	require.Equal(t, subagent.NodeFailed, node.State)
	require.Equal(t, "failed root turn", node.Error)

	second := newSessionDriver(rt, root.Clone())
	rt.subagents.mu.Lock()
	rt.subagents.bindRootDriverLocked(root.ID, rt.subagents.sessions[root.ID], second)
	rt.subagents.mu.Unlock()
	first.mu.Lock()
	require.Empty(t, first.onStarted)
	require.Empty(t, first.onSettled)
	first.mu.Unlock()
	second.mu.Lock()
	current := second.settledCallbacksLocked()
	second.mu.Unlock()
	current[0]()
	started[0]()
	settled[0]()
	node, _ = rt.subagents.tree.Node(rootID)
	require.Equal(t, subagent.NodeIdle, node.State, "callbacks captured by the old generation cannot mutate the root")
	require.Empty(t, node.Error)
	require.NoError(t, rt.subagents.CloseContext(t.Context()))
	second.mu.Lock()
	require.Empty(t, second.onStarted)
	require.Empty(t, second.onSettled)
	second.mu.Unlock()
}

func TestPublishedRootDriverBindingDoesNotCreateTopology(t *testing.T) {
	rt, _, root := newRestoreFixture(t)
	driver := newSessionDriver(rt, root)
	rt.subagents.bindPublishedRootDriver(driver)
	require.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	require.Empty(t, rt.subagents.sessions)

	rootID := subagent.SessionRootID(root.ID)
	require.NoError(t, rt.subagents.tree.Add(subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeIdle}))
	rt.subagents.sessions[root.ID] = &sessionSubagents{node: rootID, topLevel: true}
	rt.subagents.bindPublishedRootDriver(driver)
	rt.subagents.bindPublishedRootDriver(driver)
	driver.mu.Lock()
	started := driver.startedCallbacksLocked()
	driver.mu.Unlock()
	require.Len(t, started, 1)
	started[0]()
	node, _ := rt.subagents.tree.Node(rootID)
	require.Equal(t, subagent.NodeRunning, node.State)
}
