package runtime

import (
	"context"
	"strconv"
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

	rt.interactions.resumeChannel(root.ID)
	rt.interactions.resumeChannel(child.Session.ID)
	_, observer, cancel, _ := rt.sessionEvents.SubscribeSequenced(child.Session.ID, nil, 1)
	defer cancel()

	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	require.NoError(t, rt.DeleteSession(ctx, root.ID))
	for _, id := range []string{root.ID, child.Session.ID, grand.Session.ID} {
		_, exists := rt.sessionDrivers.Lookup(id)
		assert.False(t, exists)
		_, exists = rt.interactions.resume[id]
		assert.False(t, exists)
	}
	_, open := <-observer
	assert.False(t, open, "delete closes observers")
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
