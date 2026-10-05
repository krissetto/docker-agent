package runtime

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestDeleteSessionCascadesNestedSessionsInteractionsAndObservers(t *testing.T) {
	rt, _, root := newRestoreFixture(t)
	childID, err := rt.subagents.Spawn(root, "root", subagent.AllowedSubagent{Agent: "planner"}, "child")
	require.NoError(t, err)
	child, ok := rt.SubagentAttachInfo(childID)
	require.True(t, ok)
	grandID, err := rt.subagents.Spawn(child.Session, "planner", subagent.AllowedSubagent{Agent: "planner"}, "grand")
	require.NoError(t, err)
	grand, ok := rt.SubagentAttachInfo(grandID)
	require.True(t, ok)

	_, observer, cancel, _ := rt.sessionEvents.SubscribeSequenced(child.Session.ID, nil, 1)
	defer cancel()

	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	require.NoError(t, rt.DeleteSession(ctx, root.ID))
	for _, id := range []string{root.ID, child.Session.ID, grand.Session.ID} {
		_, exists := rt.sessionDrivers.Lookup(id)
		assert.False(t, exists)
	}
	_, open := <-observer
	assert.False(t, open, "delete closes observers")
	assert.Empty(t, rt.subagents.sessions)
}

func TestDeleteSessionLateMetricsPreserveNestedTopologyAndAdmissionFence(t *testing.T) {
	rt, store, root := newRestoreFixture(t)
	planner, err := rt.team.Agent("planner")
	require.NoError(t, err)
	child := session.New(session.WithID("delete-child"), session.WithAgentName("planner"))
	child.ParentID = root.ID
	require.NoError(t, rt.subagents.registerIdleChild(root, "root", child, planner, subagent.AllowedSubagent{Agent: "planner"}))
	grand := session.New(session.WithID("delete-grandchild"), session.WithAgentName("planner"))
	grand.ParentID = child.ID
	require.NoError(t, rt.subagents.registerIdleChild(child, "planner", grand, planner, subagent.AllowedSubagent{Agent: "planner"}))
	childID, ok := rt.SubagentNodeForSession(child.ID)
	require.True(t, ok)
	grandID, ok := rt.SubagentNodeForSession(grand.ID)
	require.True(t, ok)

	var observers []<-chan SequencedSessionEvent
	for _, sess := range []*session.Session{root, child, grand} {
		_, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err, "admitted rows are durable before deletion")
		_, observer, cancel, _ := rt.sessionEvents.SubscribeSequenced(sess.ID, nil, 1)
		t.Cleanup(cancel)
		observers = append(observers, observer)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	rt.subagents.mu.Lock()
	unwatch := rt.subagents.children[childID].unwatch
	rt.subagents.children[childID].unwatch = func() {
		unwatch()
		close(entered)
		<-release
	}
	rt.subagents.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	var deleteErr error
	go func() {
		deleteErr = rt.DeleteSession(ctx, root.ID)
		close(done)
	}()
	defer func() {
		unblock()
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("delete did not finish after releasing lifecycle callback")
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("delete did not reach lifecycle callback")
	}

	// Model a publisher finishing usage accounting after deletion is reserved.
	for _, sess := range []*session.Session{child, grand} {
		rt.subagents.updateSessionMetrics(sess, 1)
		_, exists := rt.SubagentTree().Node(subagent.SessionRootID(sess.ID))
		assert.False(t, exists, "late metrics must not promote a descendant to a root")
	}
	for _, parent := range []*session.Session{root, child, grand} {
		_, err := rt.subagents.Spawn(parent, "planner", subagent.AllowedSubagent{Agent: "planner"}, "rejected during deletion")
		require.Error(t, err, "deletion reserves every parent against new admissions")
	}
	for _, id := range []subagent.NodeID{childID, grandID} {
		info, exists := rt.SubagentAttachInfo(id)
		require.True(t, exists, "stopped topology remains readable while callbacks drain")
		require.NotNil(t, info.Session)
	}
	unblock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("delete did not finish")
	}
	require.NoError(t, deleteErr)
	for i, sess := range []*session.Session{root, child, grand} {
		_, exists := rt.sessionDrivers.Lookup(sess.ID)
		assert.False(t, exists)
		_, err := store.GetSession(t.Context(), sess.ID)
		require.ErrorIs(t, err, session.ErrNotFound)
		_, open := <-observers[i]
		assert.False(t, open, "delete closes observers")
	}
	assert.Empty(t, rt.subagents.sessions)
}

func TestDeleteSessionCascadeResumesAfterInterruptedDrain(t *testing.T) {
	rt, store, root := newRestoreFixture(t)
	planner, err := rt.team.Agent("planner")
	require.NoError(t, err)
	child := session.New(session.WithID("interrupted-child"), session.WithAgentName("planner"))
	child.ParentID = root.ID
	require.NoError(t, rt.subagents.registerIdleChild(root, "root", child, planner, subagent.AllowedSubagent{Agent: "planner"}))
	grand := session.New(session.WithID("interrupted-grandchild"), session.WithAgentName("planner"))
	grand.ParentID = child.ID
	require.NoError(t, rt.subagents.registerIdleChild(child, "planner", grand, planner, subagent.AllowedSubagent{Agent: "planner"}))
	driver, ok := rt.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	driver.wg.Add(1)
	release := sync.OnceFunc(driver.wg.Done)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, rt.DeleteSession(ctx, root.ID), context.Canceled)
	for _, sess := range []*session.Session{child, grand} {
		rt.subagents.updateSessionMetrics(sess, 1)
		_, exists := rt.SubagentTree().Node(subagent.SessionRootID(sess.ID))
		assert.False(t, exists)
		_, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err, "an interrupted drain retains durable descendants")
	}
	release()
	require.NoError(t, rt.DeleteSession(t.Context(), root.ID))
	for _, sess := range []*session.Session{root, child, grand} {
		_, err := store.GetSession(t.Context(), sess.ID)
		require.ErrorIs(t, err, session.ErrNotFound)
		_, exists := rt.sessionDrivers.Lookup(sess.ID)
		assert.False(t, exists)
	}
	assert.Empty(t, rt.subagents.sessions)
}

func TestDeleteSessionIsIdempotent(t *testing.T) {
	rt, sess := newSessionFixture(t)
	_, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
}

func TestNormalizeRestoredTopologyVersionAndInterruptedState(t *testing.T) {
	snapshot := subagent.Snapshot{Version: subagent.SnapshotVersion, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: "root", Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "worker", State: subagent.NodeStarting}}},
	}}}
	normalized, err := normalizeRestoredSnapshot(snapshot, subagent.DurabilityDurable)
	require.NoError(t, err)
	assert.Equal(t, subagent.NodeIdle, normalized.Nodes[0].Node.State)
	assert.Equal(t, subagent.NodeIdle, normalized.Nodes[0].Children[0].Node.State)
	assert.Equal(t, subagent.DurabilityDurable, normalized.Durability)

	snapshot.Version++
	_, err = normalizeRestoredSnapshot(snapshot, subagent.DurabilityDurable)
	require.Error(t, err, "future topology versions cannot silently downgrade")
}

func TestSessionReplayHonorsEventAndByteLimits(t *testing.T) {
	h := newSessionEventHubWithLimits(2, 1<<20)
	for i := range 4 {
		h.Publish("s", AgentChoice("a", "s", strconv.Itoa(i)))
	}
	since := uint64(0)
	seed, _, cancel, _ := h.SubscribeSequenced("s", &since, 1)
	defer cancel()
	require.Len(t, seed, 3)
	assert.True(t, seed[0].Gap)

	h = newSessionEventHubWithLimits(10, 1)
	h.Publish("s", AgentChoice("a", "s", "too large"))
	seed, _, cancel, _ = h.SubscribeSequenced("s", &since, 1)
	defer cancel()
	require.Len(t, seed, 1)
	assert.True(t, seed[0].Gap, "byte limit evicts oversized events")
	assert.Equal(t, uint64(2), seed[0].FirstAvailable)
}

func TestSharedRuntimeBindsDifferentConfiguredAgents(t *testing.T) {
	rootProvider := &mockProvider{id: "test/root", stream: newStreamBuilder().AddContent("root").AddStopWithUsage(1, 1).Build()}
	workerProvider := &mockProvider{id: "test/worker", stream: newStreamBuilder().AddContent("worker").AddStopWithUsage(1, 1).Build()}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(rootProvider)),
		agent.New("worker", "worker", agent.WithModel(workerProvider)),
	)))
	require.NoError(t, err)
	rootSession := session.New(session.WithID("root-tab"))
	workerSession := session.New(session.WithID("worker-tab"))
	rootHandle, err := rt.CreateSession(t.Context(), rootSession, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	workerHandle, err := rt.CreateSession(t.Context(), workerSession, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	assert.Equal(t, "root", rootHandle.AgentName())
	assert.Equal(t, "worker", workerHandle.AgentName())
	assert.Equal(t, "root", rt.currentAgent().Name(), "binding another session does not mutate shared current agent")
}

func TestSessionResourcePolicyZeroDisablesAdmissionAndReplay(t *testing.T) {
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionResourcePolicy(SessionResourcePolicy{}))
	require.NoError(t, err)
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("disabled")), SessionBinding{})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorCapacity, sessionErr.Kind)
	assert.Equal(t, 0, sessionErr.Limit)
	assert.Contains(t, sessionErr.Error(), "capacity limit reached (limit 0)")
}

func TestDeleteSubmissionDoesNotReportCapacityLimit(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))

	_, err = handle.Submit(t.Context(), TurnInput{Content: "after stop"})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	assert.Zero(t, sessionErr.Limit)
	assert.Equal(t, "session submit: stopped", sessionErr.Error())
}

func TestSessionByIDReturnsTypedNotFound(t *testing.T) {
	rt, _ := newSessionFixture(t)
	_, err := rt.SessionByID("missing")
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorNotFound, sessionErr.Kind)
	assert.Equal(t, "missing", sessionErr.SessionID)
	assert.Equal(t, SessionOperationLookup, sessionErr.Operation)
}

func TestDeletedSessionCannotBeRecreatedInSameRuntime(t *testing.T) {
	rt, sess := newSessionFixture(t)
	_, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))

	_, err = rt.CreateSession(t.Context(), sess, SessionBinding{})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	assert.Equal(t, SessionOperationRegister, sessionErr.Operation)
}

func TestReleasedSessionCanBeRecreatedFromDurableCopy(t *testing.T) {
	rt, sess := newSessionFixture(t)
	_, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.ReleaseSession(t.Context(), sess.ID))

	reloaded := sess.Clone()
	handle, err := rt.CreateSession(t.Context(), reloaded, SessionBinding{})
	require.NoError(t, err)
	assert.Equal(t, sess.ID, handle.ID())
}
