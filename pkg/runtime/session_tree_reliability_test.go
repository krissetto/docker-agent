package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestTreeObservationChildlessRootAndBufferOne(t *testing.T) {
	rt := newPersistedSessionRuntime(t, session.NewInMemorySessionStore())
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), root, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Tree: true, Buffer: 1})
	require.NoError(t, err)
	defer observation.Cancel()
	require.Len(t, observation.Initial, 1)
	assert.Equal(t, root.ID, observation.Initial[0].Session.ID)
	late := session.New(session.WithID("late"), session.WithParentID(root.ID), session.WithAgentName("root"))
	_, err = rt.CreateSession(t.Context(), late, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	rt.subagents.mu.Lock()
	rootTracking := rt.subagents.ensureSessionLocked(root, "root", "")
	require.NoError(t, rt.subagents.tree.Add(subagent.Node{ID: "late-node", Agent: "root", Parent: rootTracking.node, SessionID: late.ID, State: subagent.NodeIdle}))
	rt.subagents.ensureSessionLocked(late, "root", "late-node")
	rt.subagents.children["late-node"] = &childRecord{sessionID: late.ID, parentSession: root.ID, session: late, durable: session.ChildRecord{Node: subagent.Node{State: subagent.NodeIdle}}}
	rt.subagents.mu.Unlock()
	select {
	case snapshot := <-observation.SessionsAdded:
		assert.Equal(t, late.ID, snapshot.Session.ID)
	case <-time.After(time.Second):
		t.Fatal("late child snapshot not delivered")
	}
	lateDriver, ok := rt.sessionDrivers.Lookup(late.ID)
	require.True(t, ok)
	lateDriver.events.Publish(late.ID, StreamStopped(late.ID, "root", "done"))
	select {
	case event := <-observation.Events:
		assert.Equal(t, late.ID, event.SessionID)
	case <-time.After(time.Second):
		t.Fatal("late child event not delivered")
	}
}

func TestInspectSessionTreeSynthesizesChildlessRoot(t *testing.T) {
	rt := newPersistedSessionRuntime(t, session.NewInMemorySessionStore())
	root := session.New(session.WithID("inspect-root"), session.WithAgentName("root"))
	_, err := rt.CreateSession(t.Context(), root, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	view := (&localSessionRuntimeView{runtime: rt})
	snapshot, err := view.InspectSessionTree(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, snapshot.Nodes, 1)
	assert.Equal(t, subagent.SessionRootID(root.ID), snapshot.Root)
}
