package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestBackgroundProcessLifecycleIsAuthoritativePerSession(t *testing.T) {
	r := newDriverTestRuntime(t)
	first := r.sessionDrivers.Get(session.New(session.WithID("process-owner")))
	second := r.sessionDrivers.Get(session.New(session.WithID("other-owner")))
	observed := first.observe(nil, 8)
	defer observed.cancel()
	started := time.Now()
	process := tools.BackgroundProcess{ID: "job-1", SessionID: first.sessionID(), AgentName: "worker", ToolCallID: "call-1", Command: "sleep 1", Cwd: "/tmp", StartedAt: started, State: tools.BackgroundProcessRunning}
	require.NoError(t, first.adoptBackgroundProcess(t.Context(), process))
	require.Empty(t, observed.backgroundProcesses)
	select {
	case event := <-observed.live:
		payload, ok := event.Event.(*BackgroundProcessesEvent)
		require.True(t, ok)
		require.Equal(t, first.sessionID(), payload.SessionID)
		require.Len(t, payload.Processes, 1)
		require.Equal(t, process.ID, payload.Processes[0].ID)
		require.Equal(t, process.ToolCallID, payload.Processes[0].ToolCallID)
		require.True(t, payload.Processes[0].StartedAt.Equal(process.StartedAt))
	default:
		t.Fatal("adoption did not publish authoritative process replacement")
	}
	current := first.observe(nil, 8)
	defer current.cancel()
	require.Len(t, current.backgroundProcesses, 1)
	require.Equal(t, process.ID, current.backgroundProcesses[0].ID)
	require.Equal(t, process.ToolCallID, current.backgroundProcesses[0].ToolCallID)
	require.True(t, current.backgroundProcesses[0].StartedAt.Equal(process.StartedAt))
	other := second.observe(nil, 8)
	defer other.cancel()
	require.Empty(t, other.backgroundProcesses)
	finished := started.Add(time.Second)
	first.finishBackgroundProcess(t.Context(), process.ID, tools.BackgroundProcessStopped, finished)
	terminal := first.observe(nil, 8)
	defer terminal.cancel()
	require.Len(t, terminal.backgroundProcesses, 1)
	require.Equal(t, tools.BackgroundProcessStopped, terminal.backgroundProcesses[0].State)
	require.Equal(t, finished, terminal.backgroundProcesses[0].FinishedAt)
}

func TestBackgroundProcessTerminalStatePrunesWithoutTimer(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("prune")))
	process := tools.BackgroundProcess{ID: "job", SessionID: d.sessionID(), StartedAt: time.Now().Add(-10 * time.Second), State: tools.BackgroundProcessRunning}
	require.NoError(t, d.adoptBackgroundProcess(t.Context(), process))
	d.finishBackgroundProcess(t.Context(), process.ID, tools.BackgroundProcessCompleted, time.Now().Add(-5*time.Second))
	observed := d.observe(nil, 8)
	defer observed.cancel()
	require.Empty(t, observed.backgroundProcesses)
	d.mu.Lock()
	require.Empty(t, d.backgroundProcesses)
	d.mu.Unlock()
}

func TestBackgroundProcessSnapshotsAreSessionScopedAndReconnectCannotResurrect(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.subagents = newSubagentManager(r)
	rootSession := session.New(session.WithID("process-root"), session.WithAgentName("root"))
	childSession := session.New(session.WithID("process-child"), session.WithParentID(rootSession.ID), session.WithAgentName("worker"))
	rootDriver := r.sessionDrivers.Get(rootSession)
	childDriver := r.sessionDrivers.Get(childSession)
	rootHandle := &sessionHandle{runtime: r, driver: rootDriver, sessionID: rootSession.ID, agentName: "root"}
	childHandle := &sessionHandle{runtime: r, driver: childDriver, sessionID: childSession.ID, agentName: "worker"}
	rootProcess := tools.BackgroundProcess{ID: "root-job", SessionID: rootSession.ID, AgentName: "root", StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
	childProcess := tools.BackgroundProcess{ID: "child-job", SessionID: childSession.ID, AgentName: "worker", StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
	rootEpoch, rootCursor := rootDriver.events.CurrentCursor(rootSession.ID)
	childEpoch, childCursor := childDriver.events.CurrentCursor(childSession.ID)
	require.NoError(t, rootDriver.adoptBackgroundProcess(t.Context(), rootProcess))
	require.NoError(t, childDriver.adoptBackgroundProcess(t.Context(), childProcess))

	rootObservation, err := rootHandle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer rootObservation.Cancel()
	childObservation, err := childHandle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer childObservation.Cancel()
	require.Len(t, rootObservation.Primary().BackgroundProcesses, 1)
	require.Equal(t, rootSession.ID, rootObservation.Primary().BackgroundProcesses[0].SessionID)
	require.Len(t, childObservation.Primary().BackgroundProcesses, 1)
	require.Equal(t, childSession.ID, childObservation.Primary().BackgroundProcesses[0].SessionID)

	// A new observer snapshot is authoritative: old journal replacements may not roll back its cut.
	rootDriver.finishBackgroundProcess(t.Context(), rootProcess.ID, tools.BackgroundProcessCompleted, time.Now().Add(-5*time.Second))
	childDriver.finishBackgroundProcess(t.Context(), childProcess.ID, tools.BackgroundProcessStopped, time.Now().Add(-5*time.Second))
	rootReconnect, err := rootHandle.Observe(t.Context(), ObserveOptions{Since: &rootCursor, SinceEpoch: rootEpoch})
	require.NoError(t, err)
	defer rootReconnect.Cancel()
	childReconnect, err := childHandle.Observe(t.Context(), ObserveOptions{Since: &childCursor, SinceEpoch: childEpoch})
	require.NoError(t, err)
	defer childReconnect.Cancel()
	require.Empty(t, rootReconnect.Primary().BackgroundProcesses)
	require.Empty(t, childReconnect.Primary().BackgroundProcesses)
	require.NotEmpty(t, rootReconnect.Replay)
	require.NotEmpty(t, childReconnect.Replay)
	rootSeed := rootReconnect.Replay[len(rootReconnect.Replay)-1]
	childSeed := childReconnect.Replay[len(childReconnect.Replay)-1]
	require.True(t, rootSeed.IsLiveSeed())
	require.True(t, childSeed.IsLiveSeed())
	require.Empty(t, rootSeed.Event.(*BackgroundProcessesEvent).Processes)
	require.Empty(t, childSeed.Event.(*BackgroundProcessesEvent).Processes)
	for _, replay := range [][]SessionEvent{rootReconnect.Replay, childReconnect.Replay} {
		for _, event := range replay {
			if lifecycle, isLifecycle := event.Event.(*BackgroundProcessesEvent); isLifecycle {
				require.Empty(t, lifecycle.Processes, "historical lifecycle replacements are rebased to the authoritative snapshot")
			}
		}
	}
}

func TestBackgroundProcessTreeCountIdleChildAndRootIsolation(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.subagents = newSubagentManager(r)
	root := session.New(session.WithID("tree-process-root"), session.WithAgentName("root"))
	child := session.New(session.WithID("tree-process-child"), session.WithParentID(root.ID), session.WithAgentName("worker"))
	otherRoot := session.New(session.WithID("tree-process-other-root"), session.WithAgentName("other"))
	otherChild := session.New(session.WithID("tree-process-other-child"), session.WithParentID(otherRoot.ID), session.WithAgentName("worker"))
	rootDriver := r.sessionDrivers.Get(root)
	childDriver := r.sessionDrivers.Get(child)
	otherRootDriver := r.sessionDrivers.Get(otherRoot)
	otherChildDriver := r.sessionDrivers.Get(otherChild)
	r.subagents.ensureRoot(root, "root")
	r.subagents.ensureRoot(otherRoot, "other")
	firstNode, secondNode := subagent.NodeID("tree-process-child-node"), subagent.NodeID("tree-process-other-node")
	require.NoError(t, r.subagents.tree.Add(subagent.Node{ID: firstNode, Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "worker", State: subagent.NodeIdle}))
	require.NoError(t, r.subagents.tree.Add(subagent.Node{ID: secondNode, Parent: subagent.SessionRootID(otherRoot.ID), SessionID: otherChild.ID, Agent: "worker", State: subagent.NodeIdle}))
	r.subagents.mu.Lock()
	r.subagents.sessions[child.ID] = &sessionSubagents{node: firstNode}
	r.subagents.sessions[otherChild.ID] = &sessionSubagents{node: secondNode}
	r.subagents.children[firstNode] = &childRecord{sessionID: child.ID, parentSession: root.ID, durable: session.ChildRecord{Node: subagent.Node{ID: firstNode, SessionID: child.ID, State: subagent.NodeIdle}}}
	r.subagents.children[secondNode] = &childRecord{sessionID: otherChild.ID, parentSession: otherRoot.ID, durable: session.ChildRecord{Node: subagent.Node{ID: secondNode, SessionID: otherChild.ID, State: subagent.NodeIdle}}}
	r.subagents.mu.Unlock()
	rootObservation := rootDriver.observe(nil, 8)
	defer rootObservation.cancel()
	otherObservation := otherRootDriver.observe(nil, 8)
	defer otherObservation.cancel()
	process := func(id, sid, agent string) tools.BackgroundProcess {
		return tools.BackgroundProcess{ID: id, SessionID: sid, AgentName: agent, StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
	}
	first := process("job-one", child.ID, "worker")
	second := process("job-two", child.ID, "worker")
	require.NoError(t, childDriver.adoptBackgroundProcess(t.Context(), first))
	require.Eventually(t, func() bool {
		node, ok := r.subagents.tree.Node(firstNode)
		return ok && node.BackgroundProcesses == 1
	}, time.Second, time.Millisecond)
	assertRootTreeCountEvent(t, rootObservation.live, firstNode, 1)
	require.Equal(t, subagent.NodeIdle, mustSubagentNode(t, r.subagents.tree, firstNode).State, "background process does not change the idle agent lifecycle state")
	require.Equal(t, 0, mustSubagentNode(t, r.subagents.tree, secondNode).BackgroundProcesses, "other root remains isolated")
	require.NoError(t, childDriver.adoptBackgroundProcess(t.Context(), second))
	require.Eventually(t, func() bool {
		node, ok := r.subagents.tree.Node(firstNode)
		return ok && node.BackgroundProcesses == 2
	}, time.Second, time.Millisecond)
	childDriver.finishBackgroundProcess(t.Context(), first.ID, tools.BackgroundProcessCompleted, time.Now())
	require.Eventually(t, func() bool {
		node, ok := r.subagents.tree.Node(firstNode)
		return ok && node.BackgroundProcesses == 1
	}, time.Second, time.Millisecond)
	childDriver.finishBackgroundProcess(t.Context(), second.ID, tools.BackgroundProcessStopped, time.Now())
	require.Eventually(t, func() bool {
		node, ok := r.subagents.tree.Node(firstNode)
		return ok && node.BackgroundProcesses == 0
	}, time.Second, time.Millisecond)
	assertRootTreeCountEvent(t, rootObservation.live, firstNode, 0)
	require.Equal(t, subagent.NodeIdle, mustSubagentNode(t, r.subagents.tree, firstNode).State)
	require.Equal(t, 0, mustSubagentNode(t, r.subagents.tree, secondNode).BackgroundProcesses)
	_ = otherChildDriver
	_ = rootObservation
	_ = otherObservation
}

func mustSubagentNode(t *testing.T, tree *subagent.Tree, id subagent.NodeID) subagent.Node {
	t.Helper()
	node, ok := tree.Node(id)
	require.True(t, ok)
	return node
}

func TestBackgroundProcessReconnectBoundsReplacementSeed(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("bounded-process-reconnect")))
	epoch, cursor := d.events.CurrentCursor(d.sessionID())
	for range maxSessionEventSubscriberBuffer {
		d.events.Publish(d.sessionID(), Warning("replay", "root"))
	}
	observed := d.observe(&cursor, 1)
	defer observed.cancel()
	require.Len(t, observed.seed, 1)
	require.True(t, observed.seed[0].Gap)
	require.Equal(t, epoch, observed.seed[0].Epoch)
}

func TestRestoreClearsPersistedEphemeralProcessCounts(t *testing.T) {
	snapshot := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: "root:restore", Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: "root:restore", Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: "root:restore", SessionID: "child-session", Agent: "worker", State: subagent.NodeIdle, BackgroundProcesses: 4}}},
	}}}
	normalized, err := normalizeRestoredSnapshot(snapshot, subagent.DurabilityDurable)
	require.NoError(t, err)
	require.Zero(t, normalized.Nodes[0].Children[0].Node.BackgroundProcesses)
	tree := subagent.NewTree()
	require.NoError(t, tree.AddSubtree([]subagent.Node{normalized.Nodes[0].Node, normalized.Nodes[0].Children[0].Node}))
	require.Zero(t, mustSubagentNode(t, tree, "child").BackgroundProcesses)
}

func assertRootTreeCountEvent(t *testing.T, events <-chan SessionEvent, child subagent.NodeID, count int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case envelope, open := <-events:
			require.True(t, open, "root observer remains open")
			event, ok := envelope.Event.(*SubagentTreeEvent)
			if !ok {
				continue
			}
			var found *subagent.Node
			var walk func([]subagent.NodeSnapshot)
			walk = func(nodes []subagent.NodeSnapshot) {
				for i := range nodes {
					if nodes[i].Node.ID == child {
						node := nodes[i].Node
						found = &node
					}
					walk(nodes[i].Children)
				}
			}
			walk(event.Snapshot.Nodes)
			if found != nil && found.BackgroundProcesses == count {
				return
			}
		case <-timer.C:
			t.Fatalf("root did not receive subtree count %d for child %s", count, child)
		}
	}
}

func TestTailHistoryPageBudgetIncludesBackgroundProcesses(t *testing.T) {
	processes := make([]tools.BackgroundProcess, 4)
	for i := range processes {
		processes[i] = tools.BackgroundProcess{ID: fmt.Sprintf("job-%d", i), SessionID: "s", Command: strings.Repeat("x", 4096), Cwd: strings.Repeat("y", 4096), StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
	}
	page := SessionSnapshot{Projection: "tail", Session: session.New(session.WithID("s")), BackgroundProcesses: processes, Status: SessionStatus{SessionID: "s"}}
	wire := historyPageWire(page)
	data, err := json.Marshal(wire)
	require.NoError(t, err)
	require.Greater(t, len(data), 16<<10)
	require.False(t, historyPageFits(page, 16<<10), "tail response byte budgeting includes process metadata")
	withoutProcesses := page
	withoutProcesses.BackgroundProcesses = nil
	require.True(t, historyPageFits(withoutProcesses, 16<<10))
}
