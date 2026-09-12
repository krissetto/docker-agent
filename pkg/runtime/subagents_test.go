package runtime

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// newTestSubagentManager builds a manager bound to a minimal LocalRuntime. It
// does not start any real sub-sessions.
func newTestSubagentManager(t *testing.T) *subagentManager {
	t.Helper()
	r := &LocalRuntime{
		ctx:           func() context.Context { return context.WithoutCancel(t.Context()) },
		subagentStore: subagent.NewInMemoryStore(),
		policy:        DefaultSessionResourcePolicy(),
	}
	r.applyResourcePolicy()
	r.sessionEvents = newSessionEventHubWithLimits(r.maxReplayEvents, r.maxReplayBytes)
	m := &subagentManager{
		r:        r,
		tree:     subagent.NewTree(),
		ctx:      t.Context(),
		sessions: map[string]*sessionSubagents{},
		children: map[subagent.NodeID]*childRecord{},
	}
	r.subagents = m
	r.interactions = newSessionInteractions()
	r.elicitationWaiters = elicitationWaiters{}
	r.sessionDrivers = newSessionDriverRegistry(r)
	return m
}

// registerChild wires a live child into the manager as Spawn would, without
// running a real sub-session.
func (m *subagentManager) registerChild(parent *session.Session, parentAgent string, id subagent.NodeID, name string, childSess *session.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureSessionLocked(parent, parentAgent, "")
	_ = m.tree.Add(subagent.Node{ID: id, Agent: name, Parent: st.node, SessionID: childSess.ID, State: subagent.NodeRunning})
	m.ensureSessionLocked(childSess, name, id)
	var d *sessionDriver
	if m.r.sessionDrivers != nil {
		d = m.r.sessionDrivers.Get(childSess)
		d.mu.Lock()
		d.running = true
		d.openSettledLocked()
		d.mu.Unlock()
	}
	m.children[id] = &childRecord{
		name:          name,
		parentSession: parent.ID,
		sessionID:     childSess.ID,
		session:       childSess,
		state:         subagent.NodeRunning,
	}
	if d != nil {
		childID := id
		d.SetPreStartErrorGate(func() error { return m.admitChildRun(childID) }, func() { m.abortChildStart(childID) })
		unwatchSettled := d.OnSettled(func() { m.reportChildSettled(childID) })
		unwatchStarted := d.OnStarted(func() { m.markChildRunning(childID) })
		m.children[id].unwatch = func() {
			unwatchSettled()
			unwatchStarted()
		}
	}
}

func drainDriverMessages(r *LocalRuntime, sess *session.Session) []QueuedMessage {
	return r.sessionDrivers.Get(sess).DrainPending()
}

func requireOneDriverMessage(t *testing.T, r *LocalRuntime, sess *session.Session) string {
	t.Helper()
	msgs := drainDriverMessages(r, sess)
	require.Len(t, msgs, 1)
	return msgs[0].Content
}

func TestSubagentManagerDeliversTurnReportToParentReceiver(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "aaaaa", "worker", child)

	m.children["aaaaa"].result = "the answer is 42"
	m.children["aaaaa"].state = subagent.NodeIdle
	m.reportTurn("aaaaa", subagent.NodeIdle, "")

	env := requireOneDriverMessage(t, m.r, parent)
	assert.Contains(t, env, "worker")
	assert.Contains(t, env, "finished its turn")
	// Short responses travel whole, explicitly marked as full.
	assert.Contains(t, env, `Full response: "the answer is 42"`)
	assert.NotContains(t, env, "[...]")
	assert.True(t, strings.HasPrefix(env, "<system_info>"), "report wrapped in system_info")

	require.NotNil(t, parent.SubagentTree, "tree snapshot mirrored onto the top-level session for live access")
	stored, err := m.r.subagentStore.LoadTree(t.Context(), "parent")
	require.NoError(t, err)
	require.NotNil(t, stored, "snapshot written to the subagent store on state change")
	found := false
	for _, root := range stored.Nodes {
		if root.Node.ID == subagent.SessionRootID("parent") {
			require.Len(t, root.Children, 1)
			assert.Equal(t, subagent.NodeIdle, root.Children[0].Node.State)
			found = true
		}
	}
	assert.True(t, found, "snapshot contains the parent session's root")

	rec, ok := m.Read("aaaaa")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeIdle, rec.state)
	assert.Equal(t, "the answer is 42", rec.result)
}

func TestPersistSnapshotIsScopedPerRootSession(t *testing.T) {
	m := newTestSubagentManager(t)
	parentA := session.New(session.WithID("parent-a"))
	parentB := session.New(session.WithID("parent-b"))
	m.registerChild(parentA, "root", "aaaaa", "worker-a", session.New(session.WithID("child-a")))
	m.registerChild(parentB, "root", "bbbbb", "worker-b", session.New(session.WithID("child-b")))

	m.persistSnapshot()

	for _, tc := range []struct {
		parent *session.Session
		root   subagent.NodeID
		child  subagent.NodeID
	}{
		{parentA, subagent.SessionRootID(parentA.ID), "aaaaa"},
		{parentB, subagent.SessionRootID(parentB.ID), "bbbbb"},
	} {
		snapshot := tc.parent.GetSubagentTree()
		require.NotNil(t, snapshot)
		assert.Equal(t, tc.root, snapshot.Root)
		require.Len(t, snapshot.Nodes, 1)
		assert.Equal(t, tc.root, snapshot.Nodes[0].Node.ID)
		require.Len(t, snapshot.Nodes[0].Children, 1)
		assert.Equal(t, tc.child, snapshot.Nodes[0].Children[0].Node.ID)

		stored, err := m.r.subagentStore.LoadTree(t.Context(), tc.parent.ID)
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Equal(t, tc.root, stored.Root)
		require.Len(t, stored.Nodes, 1)
	}
}

func TestStoppedParentRejectsNewDescendant(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"))
	m.registerChild(root, "root", "aaaaa", "worker", child)
	m.children["aaaaa"].state = subagent.NodeStopped

	id, err := m.Spawn(child, "worker", subagent.AllowedSubagent{Agent: "worker"}, "too late")
	require.Error(t, err)
	assert.Empty(t, id)
	snapshot := m.tree.Snapshot()
	require.Len(t, snapshot.Nodes, 1)
	require.Len(t, snapshot.Nodes[0].Children, 1)
	assert.Empty(t, snapshot.Nodes[0].Children[0].Children)
}

func TestSubagentManagerSendToChildRoutesToChildReceiver(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "bbbbb", "worker", child)

	name, err := m.sendToChild("parent", "bbbbb", "keep going")
	require.NoError(t, err)
	assert.Equal(t, "worker", name)
	assert.Equal(t, "keep going", requireOneDriverMessage(t, m.r, child))
}

func TestSubagentManagerSendToChildLifecycle(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "ccccc", "worker", child)
	_, err := m.sendToChild("parent", "zzzzz", "hi")
	require.Error(t, err, "unknown id rejected")
	_, err = m.sendToChild("someone-else", "ccccc", "hi")
	require.Error(t, err, "foreign parent rejected")

	// A subagent that finished a turn stays conversational.
	m.children["ccccc"].result = "done"
	m.children["ccccc"].state = subagent.NodeIdle
	m.reportTurn("ccccc", subagent.NodeIdle, "")
	_, err = m.sendToChild("parent", "ccccc", "follow-up")
	require.NoError(t, err, "idle subagents accept follow-ups")

	// Only an explicit stop finalizes it.
	name, err := m.stopChild("parent", "ccccc")
	require.NoError(t, err)
	assert.Equal(t, "worker", name)
	_, err = m.sendToChild("parent", "ccccc", "hi")
	require.Error(t, err, "stopped subagent rejected")
	_, err = m.stopChild("parent", "ccccc")
	require.Error(t, err, "double stop rejected")

	// The record stays readable after the stop.
	rec, ok := m.Read("ccccc")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, rec.state)
	assert.Equal(t, "done", rec.result)
}

func TestSubagentManagerDeepTreeLinkage(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root-sess"))
	child := session.New(session.WithID("child-sess"))
	grandchild := session.New(session.WithID("gc-sess"))

	m.registerChild(root, "root", "ccccc", "coder", child)
	m.registerChild(child, "coder", "ddddd", "helper", grandchild)

	snap := m.tree.Snapshot()
	require.Len(t, snap.Nodes, 1, "single root")
	rootNode := snap.Nodes[0]
	require.Len(t, rootNode.Children, 1)
	childNode := rootNode.Children[0]
	assert.Equal(t, subagent.NodeID("ccccc"), childNode.Node.ID)
	require.Len(t, childNode.Children, 1, "grandchild linked under child, not a new root")
	assert.Equal(t, subagent.NodeID("ddddd"), childNode.Children[0].Node.ID)
}

func TestStopSubagentCascadesToDescendants(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root-sess"))
	child := session.New(session.WithID("child-sess"))
	grandchild := session.New(session.WithID("gc-sess"))

	m.registerChild(root, "root", "ccccc", "coder", child)
	m.registerChild(child, "coder", "ddddd", "helper", grandchild)

	_, err := m.stopChild("root-sess", "ccccc")
	require.NoError(t, err)

	for _, id := range []subagent.NodeID{"ccccc", "ddddd"} {
		rec, ok := m.Read(id)
		require.True(t, ok)
		assert.Equal(t, subagent.NodeStopped, rec.state, "%s stopped", id)
		node, ok := m.tree.Node(id)
		require.True(t, ok)
		assert.Equal(t, subagent.NodeStopped, node.State, "%s tree state stopped", id)
	}
	_, err = m.sendToChild("child-sess", "ddddd", "hi")
	require.Error(t, err, "stopped descendant rejects input")
}

func TestSubagentManagerMarksChildRunningWhenDriverWakes(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	tm := team.New(team.WithAgents(agent.New("root", "prompt",
		agent.WithModel(&activeRootBlockingProvider{id: "test/mock-model", release: release}))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	rt.subagents.ctx = t.Context()
	rt.subagents.tree = subagent.NewTree()
	rt.subagents.sessions = map[string]*sessionSubagents{}
	rt.subagents.children = map[subagent.NodeID]*childRecord{}
	rt.subagentStore = subagent.NewInMemoryStore()

	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"), session.WithAgentName("root"))
	rt.subagents.registerChild(parent, "root", "eeeee", "worker", child)
	d := rt.sessionDrivers.Get(child)
	d.mu.Lock()
	d.running = false
	d.wakeRunning = false
	d.closeSettledLocked()
	d.mu.Unlock()

	rt.subagents.children["eeeee"].state = subagent.NodeIdle
	require.NoError(t, rt.subagents.tree.Update("eeeee", func(n *subagent.Node) { n.State = subagent.NodeIdle }))
	// registerChild installs the same manager admission gate used by production;
	// assert the fixture did not bypass the Starting -> Running handshake.
	require.NotNil(t, d.preStart)

	require.True(t, rt.subagents.deliver("child", "wake up"))
	require.Eventually(t, func() bool {
		rec, ok := rt.subagents.Read("eeeee")
		return ok && rec.state == subagent.NodeRunning
	}, 2*time.Second, 10*time.Millisecond)
	node, ok := rt.subagents.tree.Node("eeeee")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeRunning, node.State)
	require.NotNil(t, parent.SubagentTree)
	stored, err := rt.subagentStore.LoadTree(t.Context(), "parent")
	require.NoError(t, err)
	require.NotNil(t, stored)
}

func TestDeliverMessageReturnsFalseWhenNoReceiver(t *testing.T) {
	m := newTestSubagentManager(t)
	assert.False(t, m.deliver("nobody", "hi"))
}

func TestRenderTranscriptLimit(t *testing.T) {
	sess := session.New(session.WithID("s"))
	sess.AddMessage(session.UserMessage("first"))
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "second"}))
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "third"}))

	full := renderTranscript(sess, 0)
	assert.Contains(t, full, "first")
	assert.Contains(t, full, "second")
	assert.Contains(t, full, "third")

	last := renderTranscript(sess, 1)
	assert.NotContains(t, last, "first")
	assert.Contains(t, last, "third")
}

// Long responses are previewed with a trailing "[...]" truncation marker (the
// signal that read_subagent has more); failures preview the error instead.
func TestTurnReportPreviews(t *testing.T) {
	newEnv := func(result, errMsg string, state subagent.NodeState) string {
		m := newTestSubagentManager(t)
		parent := session.New(session.WithID("parent"))
		child := session.New(session.WithID("child"))
		m.registerChild(parent, "root", "aaaaa", "worker", child)
		m.children["aaaaa"].result = result
		m.children["aaaaa"].state = state
		m.reportTurn("aaaaa", state, errMsg)
		return requireOneDriverMessage(t, m.r, parent)
	}

	t.Run("long response is truncated with marker", func(t *testing.T) {
		long := strings.Repeat("all work and no play ", 10)
		env := newEnv(long, "", subagent.NodeIdle)
		assert.Contains(t, env, "Full response preview:")
		assert.Contains(t, env, `[...]`)
		assert.NotContains(t, env, long, "the whole response must not be embedded")
	})

	t.Run("multiline response is collapsed to one line", func(t *testing.T) {
		env := newEnv("line one\nline two", "", subagent.NodeIdle)
		assert.Contains(t, env, `"line one line two"`)
	})

	t.Run("failed turn previews the error", func(t *testing.T) {
		env := newEnv("stale result", "model exploded", subagent.NodeFailed)
		assert.Contains(t, env, "failed")
		assert.Contains(t, env, `Error: "model exploded"`)
		assert.NotContains(t, env, "stale result")
	})

	t.Run("failed turn without error detail still reports", func(t *testing.T) {
		env := newEnv("", "", subagent.NodeFailed)
		assert.Contains(t, env, "failed")
		assert.NotContains(t, env, "Error:")
	})

	t.Run("empty successful response reports quiet turn", func(t *testing.T) {
		m := newTestSubagentManager(t)
		parent := session.New(session.WithID("parent"))
		child := session.New(session.WithID("child"))
		m.registerChild(parent, "root", "aaaaa", "worker", child)
		m.children["aaaaa"].state = subagent.NodeIdle
		m.reportTurn("aaaaa", subagent.NodeIdle, "")
		env := requireOneDriverMessage(t, m.r, parent)
		assert.Contains(t, env, "finished its turn")
		assert.NotContains(t, env, "Full response")
	})
}

// Quiescence gating: a subagent's turn end is reported to its parent only
// when no subagents of its own are still running — the delegation wave in a
// deep chain stays silent instead of waking every ancestor per leaf event.
func TestReportTurnQuiescenceGating(t *testing.T) {
	setup := func() (*subagentManager, *session.Session, *session.Session) {
		m := newTestSubagentManager(t)
		parent := session.New(session.WithID("parent"))
		child := session.New(session.WithID("child-sess"))
		grand := session.New(session.WithID("grand-sess"))
		m.registerChild(parent, "root", "aaaaa", "worker", child)
		m.registerChild(child, "worker", "bbbbb", "helper", grand)
		return m, parent, grand
	}

	t.Run("suppressed while its own subagent is running", func(t *testing.T) {
		m, parent, _ := setup()
		// helper (bbbbb) is running beneath worker; worker's turn end is
		// bookkeeping, not news.
		m.children["aaaaa"].state = subagent.NodeIdle
		m.reportTurn("aaaaa", subagent.NodeIdle, "")
		assert.Empty(t, drainDriverMessages(m.r, parent))
		// The tree still records the state change for the UI.
		n, ok := m.tree.Node("aaaaa")
		require.True(t, ok)
		assert.Equal(t, subagent.NodeIdle, n.State)
	})

	t.Run("suppressed while descendant subagent is running", func(t *testing.T) {
		m, parent, grand := setup()
		leaf := session.New(session.WithID("leaf-sess"))
		m.registerChild(grand, "helper", "ccccc", "leaf", leaf)
		m.children["bbbbb"].state = subagent.NodeIdle
		m.children["aaaaa"].state = subagent.NodeIdle
		m.reportTurn("aaaaa", subagent.NodeIdle, "")
		assert.Empty(t, drainDriverMessages(m.r, parent))
	})

	t.Run("delivered once the subtree is quiet", func(t *testing.T) {
		m, parent, _ := setup()
		m.children["bbbbb"].state = subagent.NodeIdle // helper settled
		m.children["aaaaa"].state = subagent.NodeIdle
		m.children["aaaaa"].result = "all done"
		m.reportTurn("aaaaa", subagent.NodeIdle, "")
		env := requireOneDriverMessage(t, m.r, parent)
		assert.Contains(t, env, "finished its turn")
		assert.Contains(t, env, "all done")
	})

	t.Run("failed turn waits for running subtree", func(t *testing.T) {
		m, parent, _ := setup()
		m.children["aaaaa"].state = subagent.NodeFailed
		m.reportTurn("aaaaa", subagent.NodeFailed, "model exploded")
		assert.Empty(t, drainDriverMessages(m.r, parent))
	})

	t.Run("failed turn reports once subtree is quiet", func(t *testing.T) {
		m, parent, _ := setup()
		m.children["bbbbb"].state = subagent.NodeIdle
		m.children["aaaaa"].state = subagent.NodeFailed
		m.reportTurn("aaaaa", subagent.NodeFailed, "model exploded")
		env := requireOneDriverMessage(t, m.r, parent)
		assert.Contains(t, env, "failed")
		assert.Contains(t, env, "model exploded")
	})
}

func TestReadSubagentDeniesAnotherRootChild(t *testing.T) {
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)

	rootA := session.New(session.WithID("root-a"))
	rootB := session.New(session.WithID("root-b"))
	spawn := tools.ToolCall{Function: tools.FunctionCall{
		Name:      subagent.ToolSpawnSubagent,
		Arguments: `{"agent":"planner","task":"root A secret"}`,
	}}
	spawnResult, err := rt.toolMap[subagent.ToolSpawnSubagent](t.Context(), rootA, spawn, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, spawnResult.IsError, spawnResult.Output)

	match := regexp.MustCompile(`\(([0-9a-f]{5})\)`).FindStringSubmatch(spawnResult.Output)
	require.Len(t, match, 2, "spawn output preserves the five-hex-character subagent id")
	read := tools.ToolCall{Function: tools.FunctionCall{
		Name:      subagent.ToolReadSubagent,
		Arguments: `{"subagent_id":"` + match[1] + `","full":true}`,
	}}

	denied, err := rt.toolMap[subagent.ToolReadSubagent](t.Context(), rootB, read, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, denied.IsError)
	assert.Contains(t, denied.Output, "not one of yours")
	assert.NotContains(t, denied.Output, "root A secret")

	allowed, err := rt.toolMap[subagent.ToolReadSubagent](t.Context(), rootA, read, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, allowed.IsError, allowed.Output)
	assert.Contains(t, allowed.Output, "root A secret")
}

func TestSendMessageRestoresChildAfterCapacityReclamation(t *testing.T) {
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = 1
	policy.IdleRetention = 0
	store := session.NewInMemorySessionStore()
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("root").AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("child").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionResourcePolicy(policy))
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)

	root := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(t.Context(), root))
	spawn := tools.ToolCall{Function: tools.FunctionCall{
		Name:      subagent.ToolSpawnSubagent,
		Arguments: `{"agent":"planner","task":"first turn"}`,
	}}
	spawned, err := rt.toolMap[subagent.ToolSpawnSubagent](t.Context(), root, spawn, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, spawned.IsError, spawned.Output)
	match := regexp.MustCompile(`\(([0-9a-f]{5})\)`).FindStringSubmatch(spawned.Output)
	require.Len(t, match, 2)
	id := subagent.NodeID(match[1])
	info, ok := rt.SubagentAttachInfo(id)
	require.True(t, ok)
	require.Eventually(t, func() bool { return rt.sessionDrivers.Settled(info.Session.ID) }, 2*time.Second, 10*time.Millisecond)

	pressure := session.New(session.WithID("pressure"), session.WithAgentName("root"))
	require.Eventually(t, func() bool {
		_, err = rt.CreateSession(t.Context(), pressure, SessionBinding{})
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	_, retained := rt.sessionDrivers.Lookup(info.Session.ID)
	require.False(t, retained, "capacity pressure must reclaim the settled child driver")

	send := tools.ToolCall{Function: tools.FunctionCall{
		Name:      subagent.ToolSendMessage,
		Arguments: `{"to":"` + string(id) + `","message":"second turn"}`,
	}}
	result, err := rt.toolMap[subagent.ToolSendMessage](t.Context(), root, send, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	assert.Contains(t, result.Output, string(id))
	require.Eventually(t, func() bool {
		rebound, exists := rt.sessionDrivers.Lookup(info.Session.ID)
		return exists && strings.Contains(renderTranscript(rebound.session(), 0), "second turn")
	}, 2*time.Second, 10*time.Millisecond)
}

func TestSendMessageRestoresVolatileChildFromLiveSnapshot(t *testing.T) {
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = 1
	policy.IdleRetention = 0
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/root", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt", agent.WithModel(&mockProvider{id: "test/planner", stream: newStreamBuilder().AddContent("settled answer").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionResourcePolicy(policy))
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)
	root := session.New(session.WithID("volatile-root"))
	id, err := rt.subagents.Spawn(root, "root", subagent.AllowedSubagent{Agent: "planner"}, "first turn")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rec, ok := rt.subagents.Read(id)
		return ok && rec.state == subagent.NodeIdle && strings.Contains(renderTranscript(rec.session, 0), "settled answer")
	}, 2*time.Second, 10*time.Millisecond)
	rec, ok := rt.subagents.Read(id)
	require.True(t, ok)
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
	require.NoError(t, err)
	_, exists := rt.sessionDrivers.Lookup(rec.sessionID)
	require.False(t, exists)

	result, err := rt.handleSendMessage(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{
		Name: subagent.ToolSendMessage, Arguments: `{"to":"` + string(id) + `","message":"follow up"}`,
	}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	rebound, exists := rt.sessionDrivers.Lookup(rec.sessionID)
	require.True(t, exists)
	require.Eventually(t, func() bool {
		return strings.Contains(renderTranscript(rebound.session(), 0), "follow up")
	}, 2*time.Second, 10*time.Millisecond)
	transcript := renderTranscript(rebound.session(), 0)
	assert.Contains(t, transcript, "settled answer")
	assert.Contains(t, transcript, "follow up")
	assert.True(t, rebound.session().NonInteractive)
	assert.True(t, rebound.session().AsyncSubagent)
}

// Spawning requires no embedder wiring: hosts that only call RunStream (API
// server, adapters, exec) still get working async subagents because the
// session wakes the parent for turn reports (see session_handle.go).
func TestSpawnToolNeedsNoReceiver(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)
	sess := session.New(session.WithID("parent-sess"))

	tc := tools.ToolCall{Function: tools.FunctionCall{
		Name:      subagent.ToolSpawnSubagent,
		Arguments: `{"agent":"planner","task":"analyze the codebase"}`,
	}}

	res, err := rt.handleSpawnSubagent(t.Context(), sess, tc, nil, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, res.IsError, res.Output)
	assert.Contains(t, res.Output, "Spawned subagent")
	assert.Contains(t, res.Output, "finish your response to wait")
	assert.Contains(t, res.Output, "do not poll")
}

func TestSpawnedSubagentInheritsSafetySettings(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)

	parent := session.New(
		session.WithID("parent-sess"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
		session.WithPermissions(&session.PermissionsConfig{Allow: []string{"shell"}}),
	)
	id, err := rt.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "planner"}, "plan safely")
	require.NoError(t, err)
	info, ok := rt.SubagentAttachInfo(id)
	require.True(t, ok)
	assert.Equal(t, session.SafetyPolicyBalanced, info.Session.SafetyPolicy)
	require.NotNil(t, info.Session.Permissions)
	assert.Equal(t, []string{"shell"}, info.Session.Permissions.Allow)
}
