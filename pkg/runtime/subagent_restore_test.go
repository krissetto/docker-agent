package runtime

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func newRestoreFixture(t *testing.T) (*LocalRuntime, session.Store, *session.Session) {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.(*session.SQLiteSessionStore).Close() })

	prov := func() *mockProvider {
		return &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(prov()), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner", Name: "planner"}, latest.SubagentRef{Agent: "planner", Name: "alternate"})),
		agent.New("planner", "prompt", agent.WithModel(prov()), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("reviewer", "prompt", agent.WithModel(prov())),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	sess := session.New(session.WithID(t.Name() + "/parent-sess"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return rt, store, sess
}

func TestNormalCreateRejectsReservedRestoreChildWithoutSideEffects(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	first := session.New(session.WithID("reserved-child"), session.WithAgentName("planner"))
	first.ParentID = sess.ID
	pending := session.UserMessage("must remain pending")
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "reserved-turn"
	require.NoError(t, store.AddSession(t.Context(), first))
	_, err := store.AddMessage(t.Context(), first.ID, pending)
	require.NoError(t, err)
	first, err = store.GetSession(t.Context(), first.ID)
	require.NoError(t, err)
	second := session.New(session.WithID("blocked-child"))
	second.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), second))

	unrelated := session.New(session.WithID("unrelated"), session.WithAgentName("root"))
	unrelatedDriver, err := rt.sessionDrivers.GetInitialized(t.Context(), unrelated)
	require.NoError(t, err)
	rt.idleRetention = time.Nanosecond
	unrelatedDriver.mu.Lock()
	unrelatedDriver.lastActive = time.Time{}
	unrelatedDriver.mu.Unlock()

	entered, release := make(chan struct{}), make(chan struct{})
	rt.sessionDrivers.prepareRestoreHook = func(id string) {
		if id == second.ID {
			close(entered)
			<-release
		}
	}
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "first", Parent: rootID, Agent: "planner", SessionID: first.ID, State: subagent.NodeIdle}},
			{Node: subagent.Node{ID: "second", Parent: rootID, Agent: "planner", SessionID: second.ID, State: subagent.NodeIdle}},
		},
	}}}
	done := make(chan error, 1)
	go func() { _, restoreErr := rt.subagents.Restore(t.Context(), sess, snapshot); done <- restoreErr }()
	<-entered

	_, err = rt.CreateSession(t.Context(), first.Clone(), SessionBinding{AgentName: "planner"})
	require.Error(t, err)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionOperationRestoreReserved, sessionErr.Operation)
	_, visible := rt.sessionDrivers.Lookup(first.ID)
	assert.False(t, visible)
	_, unrelatedVisible := rt.sessionDrivers.Lookup(unrelated.ID)
	assert.True(t, unrelatedVisible, "reservation rejection must happen before idle pruning")
	stored, err := store.GetSession(t.Context(), first.ID)
	require.NoError(t, err)
	require.Len(t, stored.MessagesSnapshot(), 1)
	assert.True(t, stored.MessagesSnapshot()[0].Message.Pending)
	assert.Empty(t, stored.GetLastAssistantMessageContent())

	rt.maxSessions = 1
	close(release)
	require.ErrorIs(t, <-done, ErrSessionCapacity, "the unreserved second child must fail preparation")
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	_, visible = rt.sessionDrivers.Lookup(first.ID)
	assert.False(t, visible)
	_, unrelatedVisible = rt.sessionDrivers.Lookup(unrelated.ID)
	assert.True(t, unrelatedVisible)
	stored, err = store.GetSession(t.Context(), first.ID)
	require.NoError(t, err)
	require.Len(t, stored.MessagesSnapshot(), 1)
	assert.True(t, stored.MessagesSnapshot()[0].Message.Pending)
	assert.Empty(t, stored.GetLastAssistantMessageContent())
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Empty(t, rt.sessionDrivers.reservations)
	}()
}

func TestRestorePreparedDriversRemainInvisibleAndPreserveDurablePending(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	first := session.New(session.WithID("prepared-child"))
	first.ParentID = sess.ID
	pending := session.UserMessage("preserve durable pending")
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "preserved-turn"
	first.AddMessage(pending)
	require.NoError(t, store.AddSession(t.Context(), first))
	second := session.New(session.WithID("blocked-child"))
	second.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), second))
	entered, release := make(chan struct{}), make(chan struct{})
	rt.sessionDrivers.prepareRestoreHook = func(id string) {
		if id == second.ID {
			close(entered)
			<-release
		}
	}
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "first", Parent: rootID, Agent: "planner", SessionID: first.ID, State: subagent.NodeIdle}},
			{Node: subagent.Node{ID: "second", Parent: rootID, Agent: "planner", SessionID: second.ID, State: subagent.NodeIdle}},
		},
	}}}
	done := make(chan error, 1)
	go func() { _, err := rt.subagents.Restore(t.Context(), sess, snapshot); done <- err }()
	<-entered

	_, lookup := rt.sessionDrivers.Lookup(first.ID)
	assert.False(t, lookup)
	_, err := rt.SessionByID(first.ID)
	require.Error(t, err)
	assert.False(t, rt.sessionDrivers.PostKnown(t.Context(), first.ID, QueuedMessage{Content: "not accepted"}, true))

	rt.maxSessions = 1 // reject the second child before activation
	close(release)
	require.Error(t, <-done)
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	_, lookup = rt.sessionDrivers.Lookup(first.ID)
	assert.False(t, lookup)
	rt.sessionDrivers.mu.Lock()
	assert.Empty(t, rt.sessionDrivers.reservations)
	rt.sessionDrivers.mu.Unlock()
	stored, err := store.GetSession(t.Context(), first.ID)
	require.NoError(t, err)
	require.Len(t, stored.MessagesSnapshot(), 1)
	assert.True(t, stored.MessagesSnapshot()[0].Message.Pending)
}

func TestRestoreInitializationFailureLeavesPendingChildDormantAndNoTopology(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	rt.maxSessions = 1
	first := session.New(session.WithID("pending-child"))
	first.ParentID = sess.ID
	pending := session.UserMessage("must stay pending")
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "pending-turn"
	require.NoError(t, store.AddSession(t.Context(), first))
	_, err := store.AddMessage(t.Context(), first.ID, pending)
	require.NoError(t, err)
	second := session.New(session.WithID("failing-child"))
	second.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), second))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "pending", Parent: rootID, Agent: "planner", SessionID: first.ID, State: subagent.NodeIdle}},
			{Node: subagent.Node{ID: "failing", Parent: rootID, Agent: "planner", SessionID: second.ID, State: subagent.NodeIdle}},
		},
	}}}

	_, err = rt.subagents.Restore(t.Context(), sess, snapshot)
	require.Error(t, err)
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	assert.Empty(t, rt.subagents.children)
	_, firstDriver := rt.sessionDrivers.Lookup(first.ID)
	_, secondDriver := rt.sessionDrivers.Lookup(second.ID)
	assert.False(t, firstDriver)
	assert.False(t, secondDriver)
	stored, err := store.GetSession(t.Context(), first.ID)
	require.NoError(t, err)
	msgs := stored.MessagesSnapshot()
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].Message.Pending)
	assert.Empty(t, stored.GetLastAssistantMessageContent(), "pending child must not reach its provider")
}

func TestRestoreCollisionLeavesExistingRootAndObserverUntouched(t *testing.T) {
	rt, store, firstRoot := newRestoreFixture(t)
	firstChild := session.New(session.WithID("shared-child-session"))
	firstChild.ParentID = firstRoot.ID
	require.NoError(t, store.AddSession(t.Context(), firstChild))
	firstRootID := subagent.SessionRootID(firstRoot.ID)
	firstSnapshot := subagent.Snapshot{Root: firstRootID, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: firstRootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "shared-node", Parent: firstRootID, Agent: "planner", SessionID: firstChild.ID, State: subagent.NodeIdle}}},
	}}}
	_, err := rt.subagents.Restore(t.Context(), firstRoot, firstSnapshot)
	require.NoError(t, err)
	before := rt.subagents.tree.Snapshot()
	updates, cancel := rt.subagents.tree.Subscribe(4)
	defer cancel()
	<-updates // current snapshot

	secondRoot := session.New(session.WithID("second-root"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), secondRoot))
	secondChild := session.New(session.WithID("second-child"))
	secondChild.ParentID = secondRoot.ID
	require.NoError(t, store.AddSession(t.Context(), secondChild))
	secondRootID := subagent.SessionRootID(secondRoot.ID)
	secondSnapshot := subagent.Snapshot{Root: secondRootID, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: secondRootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "shared-node", Parent: secondRootID, Agent: "planner", SessionID: secondChild.ID, State: subagent.NodeIdle}}},
	}}}

	_, err = rt.subagents.Restore(t.Context(), secondRoot, secondSnapshot)
	require.Error(t, err)
	assert.Equal(t, before, rt.subagents.tree.Snapshot())
	select {
	case update := <-updates:
		t.Fatalf("observer saw partial/rollback topology: %+v", update)
	case <-time.After(20 * time.Millisecond):
	}
	_, found := rt.sessionDrivers.Lookup(secondChild.ID)
	assert.False(t, found)
}

func TestNormalizeRestoredSnapshotDoesNotStopLaterSibling(t *testing.T) {
	snapshot := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "stopped", State: subagent.NodeStopped}},
		{Node: subagent.Node{ID: "resumable", State: subagent.NodeRunning}},
	}}

	normalized, err := normalizeRestoredSnapshot(snapshot, subagent.DurabilityVolatile)
	require.NoError(t, err)
	assert.Equal(t, subagent.NodeStopped, normalized.Nodes[0].Node.State)
	assert.Equal(t, subagent.NodeIdle, normalized.Nodes[1].Node.State)
}

func TestRestorePreflightRejectsMalformedSnapshotsWithoutPublishing(t *testing.T) {
	tests := map[string]func(rootID subagent.NodeID) subagent.Snapshot{
		"wrong root": func(rootID subagent.NodeID) subagent.Snapshot {
			return subagent.Snapshot{Root: "root:other", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root"}}}}
		},
		"malformed parent": func(rootID subagent.NodeID) subagent.Snapshot {
			return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
				Node: subagent.Node{ID: rootID, Agent: "root"},
				Children: []subagent.NodeSnapshot{{Node: subagent.Node{
					ID: "child", Parent: "wrong", Agent: "planner", SessionID: "child-session",
				}}},
			}}}
		},
		"duplicate node id": func(rootID subagent.NodeID) subagent.Snapshot {
			child := subagent.NodeSnapshot{Node: subagent.Node{ID: "duplicate", Parent: rootID, Agent: "planner", SessionID: "child-session"}}
			other := child
			other.Node.SessionID = "other-session"
			return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root"}, Children: []subagent.NodeSnapshot{child, other}}}}
		},
		"duplicate session id": func(rootID subagent.NodeID) subagent.Snapshot {
			return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root"}, Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "one", Parent: rootID, Agent: "planner", SessionID: "same-session"}},
				{Node: subagent.Node{ID: "two", Parent: rootID, Agent: "planner", SessionID: "same-session"}},
			}}}}
		},
	}
	for name, makeSnapshot := range tests {
		t.Run(name, func(t *testing.T) {
			rt, _, sess := newRestoreFixture(t)
			_, err := rt.subagents.Restore(t.Context(), sess, makeSnapshot(subagent.SessionRootID(sess.ID)))
			require.Error(t, err)
			assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
			assert.Empty(t, rt.subagents.sessions)
			assert.Empty(t, rt.subagents.children)
		})
	}
}

func TestRestoreWithRemovedChildAgentDegradesToStopped(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	child := session.New(session.WithID("removed-child"))
	child.ParentID = sess.ID
	child.AddMessage(session.NewAgentMessage("removed", &chat.Message{Role: chat.MessageRoleAssistant, Content: "preserved transcript"}))
	require.NoError(t, store.AddSession(t.Context(), child))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{
			ID: "removed", Parent: rootID, Agent: "removed", SessionID: child.ID, State: subagent.NodeIdle,
		}}},
	}}}

	got, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.NoError(t, err)
	require.Len(t, got.Nodes[0].Children, 1)
	assert.Equal(t, subagent.NodeStopped, got.Nodes[0].Children[0].Node.State)
	rec, ok := rt.subagents.Read("removed")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, rec.state)
	assert.Nil(t, rec.agent)
	_, hasDriver := rt.sessionDrivers.Lookup(child.ID)
	assert.False(t, hasDriver)
	info, ok := rt.SubagentAttachInfo("removed")
	require.True(t, ok)
	assert.Equal(t, "preserved transcript", info.Session.GetLastAssistantMessageContent())
}

func TestRestoreConfigDriftStopsChildAndDescendants(t *testing.T) {
	tests := map[string]string{
		"child no longer allowed": "reviewer",
		"child agent removed":     "removed",
	}
	for name, driftedAgent := range tests {
		t.Run(name, func(t *testing.T) {
			rt, store, sess := newRestoreFixture(t)
			parent := session.New(session.WithID("drifted-parent"))
			parent.ParentID = sess.ID
			parent.AddMessage(session.NewAgentMessage(driftedAgent, &chat.Message{Role: chat.MessageRoleAssistant, Content: "parent transcript"}))
			require.NoError(t, store.AddSession(t.Context(), parent))
			descendant := session.New(session.WithID("drifted-descendant"))
			descendant.ParentID = parent.ID
			descendant.AddMessage(session.NewAgentMessage("planner", &chat.Message{Role: chat.MessageRoleAssistant, Content: "descendant transcript"}))
			require.NoError(t, store.AddSession(t.Context(), descendant))
			rootID := subagent.SessionRootID(sess.ID)
			snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
				Node: subagent.Node{ID: rootID, Agent: "root"},
				Children: []subagent.NodeSnapshot{{
					Node: subagent.Node{ID: "drifted", Parent: rootID, Agent: driftedAgent, SessionID: parent.ID, State: subagent.NodeIdle},
					Children: []subagent.NodeSnapshot{{Node: subagent.Node{
						ID: "descendant", Parent: "drifted", Agent: "planner", SessionID: descendant.ID, State: subagent.NodeRunning,
					}}},
				}},
			}}}

			got, err := rt.subagents.Restore(t.Context(), sess, snapshot)
			require.NoError(t, err)
			require.Len(t, got.Nodes[0].Children[0].Children, 1)
			assert.Equal(t, subagent.NodeStopped, got.Nodes[0].Children[0].Node.State)
			assert.Equal(t, subagent.NodeStopped, got.Nodes[0].Children[0].Children[0].Node.State)
			for _, id := range []subagent.NodeID{"drifted", "descendant"} {
				rec, ok := rt.subagents.Read(id)
				require.True(t, ok)
				assert.Equal(t, subagent.NodeStopped, rec.state)
				assert.Nil(t, rec.agent)
			}
			for _, id := range []string{parent.ID, descendant.ID} {
				_, hasDriver := rt.sessionDrivers.Lookup(id)
				assert.False(t, hasDriver)
			}
			info, ok := rt.SubagentAttachInfo("descendant")
			require.True(t, ok)
			assert.Equal(t, "descendant transcript", info.Session.GetLastAssistantMessageContent())
		})
	}
}

func TestRestorePreflightAllowsDuplicateRenamedAliasesForSameAgent(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	child := session.New(session.WithID("alias-child"))
	child.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), child))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Name: "historical-alias", Parent: rootID, Agent: "planner", SessionID: child.ID, State: subagent.NodeIdle}}},
	}}}

	_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.NoError(t, err)
}

func TestRestorePreflightAcceptsUnambiguousAliasRenameAndSessionlessFailure(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	child := session.New(session.WithID("renamed-child"))
	child.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), child))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "renamed", Name: "old-alias", Parent: rootID, Agent: "planner", SessionID: child.ID, State: subagent.NodeIdle}},
			{Node: subagent.Node{ID: "failed", Parent: rootID, Agent: "planner", State: subagent.NodeFailed, Error: "historical failure"}},
		},
	}}}

	got, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.NoError(t, err)
	require.Len(t, got.Nodes[0].Children, 2)
	failed, ok := rt.subagents.tree.Node("failed")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, failed.State)
	_, found := rt.sessionDrivers.Lookup("")
	assert.False(t, found)
}

func TestRestorePreflightRejectsConfigDriftBindingMismatch(t *testing.T) {
	parentSessionID := t.Name() + "/parent-sess"
	tests := map[string]func(rootID subagent.NodeID) (subagent.Snapshot, []*session.Session){
		"unknown agent child": func(rootID subagent.NodeID) (subagent.Snapshot, []*session.Session) {
			child := session.New(session.WithID("unknown-bound-child"))
			child.ParentID = t.Name() + "/parent-sess"
			child.SetAttribute(SessionAgentAttribute, "root")
			return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
				Node:     subagent.Node{ID: rootID, Agent: "root"},
				Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "unknown", Parent: rootID, Agent: "removed", SessionID: child.ID}}},
			}}}, []*session.Session{child}
		},
		"descendant under drift-stopped parent": func(rootID subagent.NodeID) (subagent.Snapshot, []*session.Session) {
			parent := session.New(session.WithID("drift-bound-parent"))
			parent.ParentID = t.Name() + "/parent-sess"
			descendant := session.New(session.WithID("drift-bound-descendant"))
			descendant.ParentID = parent.ID
			descendant.SetAttribute(SessionAgentAttribute, "root")
			return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
				Node: subagent.Node{ID: rootID, Agent: "root"},
				Children: []subagent.NodeSnapshot{{
					Node: subagent.Node{ID: "drifted", Parent: rootID, Agent: "removed", SessionID: parent.ID},
					Children: []subagent.NodeSnapshot{{Node: subagent.Node{
						ID: "bound-descendant", Parent: "drifted", Agent: "planner", SessionID: descendant.ID,
					}}},
				}},
			}}}, []*session.Session{parent, descendant}
		},
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			rt, store, sess := newRestoreFixture(t)
			snapshot, sessions := fixture(subagent.SessionRootID(sess.ID))
			for _, child := range sessions {
				if child.ParentID == parentSessionID {
					child.ParentID = sess.ID
				}
				require.NoError(t, store.AddSession(t.Context(), child))
			}

			_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "session binding")
			assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
			assert.Empty(t, rt.subagents.children)
		})
	}
}

func TestRestorePreflightRejectsWrongPersistedChildBinding(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	child := session.New(session.WithID("bound-child"))
	child.ParentID = sess.ID
	child.SetAttribute(SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), child))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: rootID, Agent: "planner", SessionID: child.ID}}},
	}}}

	_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session binding")
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
}

func TestRestorePreflightRejectsUnrelatedStoredSession(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	unrelatedParent := session.New(session.WithID("different-parent"))
	require.NoError(t, store.AddSession(t.Context(), unrelatedParent))
	unrelated := session.New(session.WithID("unrelated-child"))
	unrelated.ParentID = "different-parent"
	require.NoError(t, store.AddSession(t.Context(), unrelated))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: rootID, Agent: "root"},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: rootID, Agent: "planner", SessionID: unrelated.ID}}},
	}}}

	_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match enclosing session")
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	_, found := rt.sessionDrivers.Lookup(unrelated.ID)
	assert.False(t, found)
}

func TestRestoreRejectsInvalidSnapshotWithoutRuntimeState(t *testing.T) {
	rt, _, sess := newRestoreFixture(t)

	got, err := rt.subagents.Restore(t.Context(), sess, subagent.Snapshot{
		Version: subagent.SnapshotVersion + 1,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported topology version")
	assert.Empty(t, got.Nodes)
	assert.Empty(t, rt.subagents.sessions)
	assert.Empty(t, rt.subagents.children)
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
}

func TestRestoreAfterCloseCreatesNoRuntimeState(t *testing.T) {
	rt, _, sess := newRestoreFixture(t)
	require.NoError(t, rt.Close())
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{
			ID: "closed-child", Agent: "planner", Parent: rootID, SessionID: "closed-session", State: subagent.NodeIdle,
		}}},
	}}}

	got, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.NoError(t, err)
	assert.Empty(t, got.Nodes)
	assert.Empty(t, rt.subagents.sessions)
	assert.Empty(t, rt.subagents.children)
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	_, found := rt.sessionDrivers.Lookup("closed-session")
	assert.False(t, found)
	rt.subagents.persistMu.Lock()
	defer rt.subagents.persistMu.Unlock()
	assert.Nil(t, rt.subagents.persist)
}

func TestRestoreRacingCloseCannotPublishAfterCloseReturns(t *testing.T) {
	rt, _, sess := newRestoreFixture(t)
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
	}}}

	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
		assert.NoError(t, err)
	}()
	close(start)
	require.NoError(t, rt.Close())
	<-done

	before := rt.subagents.tree.Snapshot()
	_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
	require.NoError(t, err)
	assert.Equal(t, before, rt.subagents.tree.Snapshot(), "restore cannot mutate state after Close returns")
}

func TestConcurrentRestorePublishesOneRecordAndHookSet(t *testing.T) {
	rt, store, sess := newRestoreFixture(t)
	childSess := session.New(session.WithID("concurrent-child"))
	childSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), childSess))
	rootID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{
			ID: "c0c0c", Agent: "planner", Parent: rootID, SessionID: childSess.ID, State: subagent.NodeIdle,
		}}},
	}}}

	const callers = 12
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_, err := rt.subagents.Restore(t.Context(), sess, snapshot)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	rec, ok := rt.subagents.Read("c0c0c")
	require.True(t, ok)
	require.NotNil(t, rec.unwatch)
	d, ok := rt.sessionDrivers.Lookup(childSess.ID)
	require.True(t, ok)
	d.mu.Lock()
	assert.Len(t, d.onStarted, 1)
	assert.Empty(t, d.onSettled, "driver commits settlement directly without a second report authority")
	d.mu.Unlock()
	live := rt.subagents.tree.Snapshot()
	require.Len(t, live.Nodes, 1)
	require.Len(t, live.Nodes[0].Children, 1)
}

func TestRestoreSubagentTreeResumesIdleSubagents(t *testing.T) {
	t.Parallel()

	rt, store, sess := newRestoreFixture(t)

	childSess := session.New(session.WithID(t.Name() + "/child-sess"))
	childSess.ParentID = sess.ID
	childSess.AddMessage(session.NewAgentMessage("planner", &chat.Message{Role: chat.MessageRoleAssistant, Content: "the plan"}))
	require.NoError(t, store.AddSession(t.Context(), childSess))
	stoppedSess := session.New(session.WithID(t.Name() + "/stopped-sess"))
	stoppedSess.ParentID = sess.ID
	stoppedSess.AddMessage(session.NewAgentMessage("planner", &chat.Message{Role: chat.MessageRoleAssistant, Content: "stopped plan"}))
	require.NoError(t, store.AddSession(t.Context(), stoppedSess))
	stoppedChildSess := session.New(session.WithID(t.Name() + "/stopped-child-sess"))
	stoppedChildSess.ParentID = stoppedSess.ID
	stoppedChildSess.AddMessage(session.NewAgentMessage("planner", &chat.Message{Role: chat.MessageRoleAssistant, Content: "stopped child plan"}))
	require.NoError(t, store.AddSession(t.Context(), stoppedChildSess))

	rootID := subagent.SessionRootID(sess.ID)
	stored := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: t.Name() + "/child-sess", State: subagent.NodeRunning}},
			{Node: subagent.Node{ID: "00bad", Agent: "planner", Parent: rootID, SessionID: "missing-session", State: subagent.NodeIdle}}, // absent stored session: unresumable
			{
				Node: subagent.Node{ID: "55d0f", Agent: "planner", Parent: rootID, SessionID: t.Name() + "/stopped-sess", State: subagent.NodeStopped},
				Children: []subagent.NodeSnapshot{
					{Node: subagent.Node{ID: "c001d", Agent: "planner", Parent: "55d0f", SessionID: t.Name() + "/stopped-child-sess", State: subagent.NodeRunning}},
				},
			},
		},
	}}}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, stored))

	snap, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)
	require.NotNil(t, snap)

	// The resumable child was adopted as idle (a run cannot survive a
	// restart) with its record rebuilt; the unresumable one is stopped.
	live, ok := rt.subagents.tree.Node("77c88")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeIdle, live.State)
	rec, ok := rt.subagents.Read("77c88")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeIdle, rec.state)
	assert.Equal(t, "the plan", rec.result, "latest result recovered from the transcript")

	dead, ok := rt.subagents.tree.Node("00bad")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, dead.State)
	stopped, ok := rt.subagents.tree.Node("55d0f")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stopped.State, "persisted stopped subagents stay stopped even if resumable")
	stoppedRec, ok := rt.subagents.Read("55d0f")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stoppedRec.state)
	stoppedDescendant, ok := rt.subagents.tree.Node("c001d")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stoppedDescendant.State, "descendants of stopped subagents stay stopped")
	stoppedInfo, ok := rt.SubagentAttachInfo("55d0f")
	require.True(t, ok, "stopped subagents with stored sessions remain attachable")
	assert.Equal(t, t.Name()+"/stopped-sess", stoppedInfo.Session.ID)
	assert.Equal(t, "stopped plan", ownAssistantResult(stoppedInfo.Session), "parent result comes from its own row, not the linked descendant")
	stoppedChildInfo, ok := rt.SubagentAttachInfo("c001d")
	require.True(t, ok, "stopped descendants with stored sessions remain attachable")
	assert.Equal(t, t.Name()+"/stopped-child-sess", stoppedChildInfo.Session.ID)
	assert.Equal(t, "stopped child plan", ownAssistantResult(stoppedChildInfo.Session))
	stoppedDescendantRec, ok := rt.subagents.Read("c001d")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stoppedDescendantRec.state)
	require.NotNil(t, stoppedDescendantRec.session)

	// Adopted subagents accept follow-ups; stopped ones don't.
	_, err = rt.subagents.sendToChild(sess.ID, "77c88", "continue please")
	require.NoError(t, err, "adopted subagents stay conversational")
	_, err = rt.subagents.sendToChild(sess.ID, "00bad", "hello?")
	require.Error(t, err)
	_, err = rt.subagents.sendToChild(sess.ID, "55d0f", "hello?")
	require.Error(t, err)
	_, err = rt.subagents.sendToChild(t.Name()+"/stopped-sess", "c001d", "hello?")
	require.Error(t, err)

	// Idempotent for a session already tracked in-process.
	again, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)
	require.NotNil(t, again)
}

// Attaching a viewer to an adopted subagent's session: injected input is
// mirrored as a UserMessageEvent (even while the child is idle) and the
// re-run's stream events follow.
func TestSessionEventsMirrorSubagentRuns(t *testing.T) {
	t.Parallel()

	rt, store, sess := newRestoreFixture(t)

	childSess := session.New(session.WithID(t.Name() + "/child-sess"))
	childSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), childSess))

	rootID := subagent.SessionRootID(sess.ID)
	stored := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: t.Name() + "/child-sess", State: subagent.NodeIdle}},
		},
	}}}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, stored))
	_, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)

	info, ok := rt.SubagentAttachInfo("77c88")
	require.True(t, ok)
	assert.Equal(t, "planner", info.Agent)
	assert.Equal(t, sess.ID, info.ParentSessionID)
	assert.Equal(t, "root", info.ParentAgent)
	require.NotNil(t, info.Session)
	assert.Equal(t, t.Name()+"/child-sess", info.Session.ID)

	_, events, cancel := subscribeSessionEventsForTest(rt, t.Name()+"/child-sess")
	defer cancel()

	require.True(t, deliverMessageForTest(rt, t.Context(), t.Name()+"/child-sess", "keep going"))

	var sawUser, sawStop bool
	deadline := time.After(10 * time.Second)
	for !sawUser || !sawStop {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case *PendingUserMessageAcceptedEvent:
				if e.Message == "keep going" {
					sawUser = true
				}
			case *StreamStoppedEvent:
				sawStop = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for mirrored events (user=%v stop=%v)", sawUser, sawStop)
		}
	}
}

// After a reload, spawning a NEW subagent must not overwrite the persisted
// snapshot with a tree that only contains the new one: the adopted subagents
// have to survive the next persist (and therefore the next reload).
func TestRestoredSubagentsSurviveNewSpawns(t *testing.T) {
	t.Parallel()

	rt, store, sess := newRestoreFixture(t)

	childSess := session.New(session.WithID(t.Name() + "/child-sess"))
	childSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), childSess))

	rootID := subagent.SessionRootID(sess.ID)
	stored := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: t.Name() + "/child-sess", State: subagent.NodeIdle}},
		},
	}}}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, stored))

	_, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)

	// Delegate to a new subagent after the reload.
	newID, err := rt.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "new task")
	require.NoError(t, err)
	require.NotEmpty(t, newID)

	// The re-persisted snapshot must contain BOTH the adopted and the new
	// subagent.
	persisted, err := store.(*session.SQLiteSessionStore).LoadTree(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	ids := map[subagent.NodeID]bool{}
	for _, root := range persisted.Nodes {
		if root.Node.ID == rootID {
			for _, c := range root.Children {
				ids[c.Node.ID] = true
			}
		}
	}
	assert.True(t, ids["77c88"], "adopted subagent survives the next persist")
	assert.True(t, ids[newID], "new subagent persisted alongside")
}

// A spawned subagent's session reads like an ordinary session: the task is
// the first regular user message (no task-in-system-prompt, no implicit
// "Please proceed."), and parent messages arrive as plain user messages.
// Transcript viewers (attached tabs, read_subagent) depend on this shape.
func TestSpawnedSubagentSessionShape(t *testing.T) {
	t.Parallel()

	rt, _, sess := newRestoreFixture(t)

	id, err := rt.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "analyze the codebase")
	require.NoError(t, err)
	require.NotEmpty(t, id)

	info, ok := rt.SubagentAttachInfo(id)
	require.True(t, ok)

	msgs := info.Session.GetAllMessages()
	require.NotEmpty(t, msgs)
	first := msgs[0]
	assert.Equal(t, chat.MessageRoleUser, first.Message.Role, "the task is the first user message, not a system prompt")
	assert.Equal(t, "analyze the codebase", first.Message.Content)
	assert.False(t, first.Implicit, "the task must be visible to transcript viewers")
	for _, m := range msgs {
		assert.NotEqual(t, "Please proceed.", m.Message.Content, "no synthetic kick-off message")
		assert.NotEqual(t, chat.MessageRoleSystem, m.Message.Role,
			"zero session system messages: the child sees only what the parent wrote")
	}

	// A parent message lands as a plain user message — no system_info wrap.
	tc := tools.ToolCall{}
	tc.Function.Name = subagent.ToolSendMessage
	tc.Function.Arguments = `{"to":"` + string(id) + `","message":"extra context"}`
	res, err := rt.handleSendMessage(t.Context(), sess, tc, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)

	require.Eventually(t, func() bool {
		current, ok := rt.SubagentViewInfo(id)
		if !ok || current.Session == nil {
			return false
		}
		for _, m := range current.Session.GetAllMessages() {
			if m.Message.Role == chat.MessageRoleUser && m.Message.Content == "extra context" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "parent message must reach the child session unwrapped")
}

// Child sessions get their title generated from the first user message (the
// task), like any other session — no hardcoded "Subagent <name>" label. The
// SessionTitleEvent reaches attached viewers through the session event hub.
func TestChildSessionTitleGeneration(t *testing.T) {
	t.Parallel()

	rt, _, _ := newRestoreFixture(t)

	child := session.New(session.WithID("child-title-sess"))
	_, events, cancel := subscribeSessionEventsForTest(rt, child.ID)
	defer cancel()

	rt.subagents.startChildTitle(child, "analyze the codebase")

	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			titleEv, ok := ev.(*SessionTitleEvent)
			if !ok {
				continue
			}
			assert.NotEmpty(t, titleEv.Title)
			assert.NotContains(t, titleEv.Title, "Subagent ", "no hardcoded label")
			// Publish happens after the title write: safe to read now.
			assert.Equal(t, titleEv.Title, child.Title)
			return
		case <-deadline:
			t.Fatal("timed out waiting for the child's SessionTitleEvent")
		}
	}
}

// Session links resolve at any depth: every subagent sub-session (including
// grandchildren) maps back to its node, and its attach info names its own
// parent session — what the TUI's "parent:" link relies on.
func TestSubagentNodeForSessionAtDepth(t *testing.T) {
	t.Parallel()

	rt, _, sess := newRestoreFixture(t)

	childID, err := rt.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "child task")
	require.NoError(t, err)
	childInfo, ok := rt.SubagentAttachInfo(childID)
	require.True(t, ok)

	grandID, err := rt.subagents.Spawn(childInfo.Session, "planner", subagent.AllowedSubagent{Agent: "planner"}, "grandchild task")
	require.NoError(t, err)
	grandInfo, ok := rt.SubagentAttachInfo(grandID)
	require.True(t, ok)
	assert.Equal(t, childInfo.Session.ID, grandInfo.ParentSessionID, "grandchild's parent is the intermediate child session")

	// The intermediate session resolves to its node even with no tab open.
	node, ok := rt.SubagentNodeForSession(childInfo.Session.ID)
	require.True(t, ok)
	assert.Equal(t, childID, node)

	node, ok = rt.SubagentNodeForSession(grandInfo.Session.ID)
	require.True(t, ok)
	assert.Equal(t, grandID, node)

	_, ok = rt.SubagentNodeForSession("no-such-session")
	assert.False(t, ok)
}

// Startup info for a pinned session (e.g. an attached subagent tab) must
// describe the session's agent everywhere — including TeamInfoEvent's
// CurrentAgent, which the TUI uses as the selected agent.
func TestEmitStartupInfoHonoursPinnedSessionAgent(t *testing.T) {
	t.Parallel()

	rt, _, _ := newRestoreFixture(t)

	pinned := session.New(session.WithID("pinned-sess"), session.WithAgentName("planner"))
	events := make(chan Event, 64)
	go func() {
		defer close(events)
		rt.EmitStartupInfo(t.Context(), pinned, NewChannelSink(events))
	}()

	var agentName, teamCurrent string
	for ev := range events {
		switch e := ev.(type) {
		case *AgentInfoEvent:
			agentName = e.AgentName
		case *TeamInfoEvent:
			teamCurrent = e.CurrentAgent
		}
	}
	assert.Equal(t, "planner", agentName)
	assert.Equal(t, "planner", teamCurrent, "the selected agent is the session's pinned agent, not the runtime's global current agent")
}
