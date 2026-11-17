package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestCanonicalSessionDoesNotAlsoSubscribeToUnorderedGlobalTree(t *testing.T) {
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(stubProvider{})))))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithID("tree-owner"), session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	a := New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(rt))
	a.currentState.handle = handle
	a.startSubagentTreeBridge(t.Context())
	a.emitLiveSubagentTree(t.Context())
	// This global mutation has no session journal identity. A canonical view must
	// not race it against its ordered owner projection on a second subscription.
	require.NoError(t, rt.SubagentTree().Add(subagent.Node{ID: "foreign-root", Agent: "root", SessionID: "foreign"}))
	select {
	case event := <-a.events:
		t.Fatalf("canonical view received unsequenced global tree: %T", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestCanonicalResetSeedsTreeBeforeLiveAttentionTransitions(t *testing.T) {
	a := newMetadataTestApp(t)
	root := subagent.SessionRootID("root")
	tree := subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "worker", SessionID: "child", Parent: root, State: subagent.NodeFailed, NeedsAttention: true, WaitingOn: "failed", Error: "model unavailable"}}}}}}
	sess := session.New(session.WithID("root"))
	sess.SetSubagentTree(&tree)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "root"}
	sink.Reset(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "root"}})
	reset := (<-a.events).(SessionEventMsg)
	require.IsType(t, &SessionResetEvent{}, reset.Event)
	seed := (<-a.events).(SessionEventMsg)
	require.True(t, seed.Seed)
	require.Equal(t, "root", seed.OriginSessionID)
	require.IsType(t, &runtime.SubagentTreeEvent{}, seed.Event)
	require.Len(t, DescendantAttention(seed.Event.(*runtime.SubagentTreeEvent).Snapshot, "root"), 1)

	tree.Nodes[0].Children[0].Node.NeedsAttention = false
	tree.Nodes[0].Children[0].Node.State = subagent.NodeRunning
	sink.Apply(runtime.SessionEvent{SessionID: "root", Sequence: 1, Event: runtime.SubagentTree(tree)})
	live := (<-a.events).(SessionEventMsg)
	require.False(t, live.Seed)
	require.Equal(t, uint64(1), live.Sequence)
	require.Empty(t, DescendantAttention(live.Event.(*runtime.SubagentTreeEvent).Snapshot, "root"))
}

func TestCompatibilitySessionKeepsLiveGlobalTreeBridge(t *testing.T) {
	tree := subagent.NewTree()
	a := New(t.Context(), nil, session.New(session.WithID("root")), runtime.SessionBinding{}, WithRuntimeServices(treeRuntime{tree: tree}))
	a.startSubagentTreeBridge(t.Context())
	require.NoError(t, tree.Add(subagent.Node{ID: "root:root", SessionID: "root", Agent: "root"}))
	select {
	case event := <-a.events:
		require.IsType(t, &runtime.SubagentTreeEvent{}, event)
	case <-time.After(time.Second):
		t.Fatal("compatibility tree subscription did not forward a snapshot")
	}
}
