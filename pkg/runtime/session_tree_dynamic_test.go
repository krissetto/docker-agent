package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestTreeObservationLateChildAtomicSeedsBeforeLive(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	rootDriver := m.r.sessionDrivers.Get(root)
	m.ensureRoot(root, "root")
	h := &sessionHandle{runtime: m.r, driver: rootDriver, sessionID: root.ID, agentName: "root"}
	observation, err := h.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: true, Buffer: 1})
	require.NoError(t, err)
	defer observation.Cancel()
	require.Nil(t, observation.SessionsAdded)
	require.Nil(t, observation.Events)

	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAgentName("worker"))
	childDriver := m.r.sessionDrivers.Get(child)
	childDriver.events.Publish(child.ID, StreamStarted(child.ID, "worker"))
	childDriver.events.Publish(child.ID, AgentChoice("worker", child.ID, "uncommitted assistant"))
	childDriver.events.Publish(child.ID, PartialToolCall(tools.ToolCall{ID: "pending-call"}, tools.Tool{}, "worker"))
	require.NoError(t, m.tree.Add(subagent.Node{ID: "late-child", Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "worker", State: subagent.NodeRunning}))

	var baseline TreeUpdate
	select {
	case baseline = <-observation.TreeUpdates:
	case <-time.After(time.Second):
		t.Fatal("late baseline was not admitted")
	}
	require.NotNil(t, baseline.Snapshot)
	require.Nil(t, baseline.Event)
	assert.Equal(t, child.ID, baseline.Snapshot.Session.ID)
	require.Len(t, baseline.Replay, 3)
	for _, seed := range baseline.Replay {
		assert.True(t, seed.IsLiveSeed())
	}
	assert.Equal(t, "uncommitted assistant", baseline.Replay[1].Event.(*AgentChoiceEvent).Content)
	assert.Equal(t, "pending-call", baseline.Replay[2].Event.(*PartialToolCallEvent).ToolCall.ID)
	require.Empty(t, observation.Replay, "returned initial replay must not be mutated")
	childDriver.events.Publish(child.ID, AgentChoice("worker", child.ID, " live"))
	select {
	case tail := <-observation.TreeUpdates:
		require.Nil(t, tail.Snapshot)
		require.NotNil(t, tail.Event)
		assert.Greater(t, tail.Event.Sequence, baseline.Snapshot.Cursor)
		assert.Equal(t, " live", tail.Event.Event.(*AgentChoiceEvent).Content)
	case <-time.After(time.Second):
		t.Fatal("late child live event was not delivered")
	}
}
