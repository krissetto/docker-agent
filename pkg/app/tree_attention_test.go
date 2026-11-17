package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

// Descendant approvals and questions aggregate into the shared tree topology
// for views without an in-process tree, and clear when resolved elsewhere.
func TestTreeAttentionAggregatesDescendantInteractions(t *testing.T) {
	root := session.New(session.WithID("root-session"), session.WithAgentName("root"))
	tree := make(chan runtime.SessionEvent, 8)
	added := make(chan runtime.SessionSnapshot, 1)
	handle := &projectionSession{id: root.ID, events: make(chan runtime.SessionEvent), observeTree: func(context.Context) (runtime.Observation, error) {
		return runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: root, Status: runtime.SessionStatus{SessionID: root.ID}}}, SessionsAdded: added, Events: tree, Cancel: func() {}}, nil
	}}
	a := New(t.Context(), &projectionSessions{session: handle}, root, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	trees := make(chan subagent.Snapshot, 16)
	ready := make(chan struct{})
	go a.Subscribe(t.Context(), func(msg any) {
		if event, ok := msg.(*runtime.SubagentTreeEvent); ok {
			trees <- event.Snapshot
		}
	}, SubscribeOptions{Ready: ready})
	<-ready
	a.Start(t.Context())

	next := func(match func(subagent.Snapshot) bool) subagent.Snapshot {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case snapshot := <-trees:
				if match(snapshot) {
					return snapshot
				}
			case <-deadline:
				t.Fatal("expected tree projection was not published")
			}
		}
	}
	tree <- runtime.SessionEvent{SessionID: root.ID, Event: &runtime.SubagentCreatedEvent{SessionID: root.ID, ParentSessionID: root.ID, ChildSessionID: "child-session", NodeID: "c0ffe"}}
	tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.SubagentCreatedEvent{SessionID: "child-session", ParentSessionID: "child-session", ChildSessionID: "grand-session", NodeID: "beef0"}}
	added <- runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "grand-session", AgentName: "reviewer", State: runtime.SessionStateRunning}}
	tree <- runtime.SessionEvent{SessionID: "grand-session", Event: &runtime.ElicitationRequestEvent{SessionID: "grand-session", RequestID: "question"}}
	tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child-session", RequestID: "approval"}}

	snapshot := next(func(s subagent.Snapshot) bool {
		grand, ok := FindSubagentTarget(s, "beef0")
		return len(DescendantAttention(s, root.ID)) == 2 && ok && grand.Agent == "reviewer"
	})
	attention := DescendantAttention(snapshot, root.ID)
	waiting := map[string]string{}
	for _, item := range attention {
		waiting[item.SessionID] = item.WaitingOn
	}
	assert.Equal(t, map[string]string{"child-session": waitingOnApproval, "grand-session": waitingOnAnswer}, waiting)
	grand, ok := a.ResolveSubagentTarget("beef0")
	require.True(t, ok)
	assert.Equal(t, "grand-session", grand.SessionID)
	assert.Equal(t, "reviewer", grand.Agent)
	assert.Len(t, a.DescendantAttention(), 2)
	assert.Equal(t, []TreeAttention{{SubagentTarget: grand, WaitingOn: waitingOnAnswer, OccurrenceID: "interaction:question"}}, DescendantAttention(snapshot, "child-session"), "a nested view aggregates only its own subtree")

	// A second same-reason interaction arrives while the first is still pending;
	// the actual observation-to-tree projection must retain both occurrences.
	tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child-session", RequestID: "approval-two"}}
	snapshot = next(func(s subagent.Snapshot) bool { return len(DescendantAttention(s, root.ID)) == 3 })
	var childOccurrences []string
	for _, item := range DescendantAttention(snapshot, root.ID) {
		if item.SessionID == "child-session" {
			childOccurrences = append(childOccurrences, item.OccurrenceID)
		}
	}
	assert.Equal(t, []string{"interaction:approval", "interaction:approval-two"}, childOccurrences)
	tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.InteractionResolvedEvent{SessionID: "child-session", InteractionID: "approval"}}
	snapshot = next(func(s subagent.Snapshot) bool { return len(DescendantAttention(s, root.ID)) == 2 })
	tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.InteractionResolvedEvent{SessionID: "child-session", InteractionID: "approval-two"}}
	snapshot = next(func(s subagent.Snapshot) bool { return len(DescendantAttention(s, root.ID)) == 1 })
	assert.Equal(t, "grand-session", DescendantAttention(snapshot, root.ID)[0].SessionID)

	// Replay and the ordered live stream may repeat a resolved prompt. It must
	// not emit a new tree attention transition or re-open the old dialog.
	for range 20 {
		tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child-session", RequestID: "approval"}}
	}
	tree <- runtime.SessionEvent{SessionID: "grand-session", Event: &runtime.InteractionResolvedEvent{SessionID: "grand-session", InteractionID: "question"}}
	snapshot = next(func(s subagent.Snapshot) bool { return len(DescendantAttention(s, root.ID)) == 0 })
	assert.Empty(t, DescendantAttention(snapshot, root.ID))
}

func TestTreeAttentionIsNotObservedForInProcessHandles(t *testing.T) {
	watch := newTreeWatch("root")
	assert.Nil(t, watch.merged(nil), "no topology without observed descendants")
	assert.False(t, watch.apply(runtime.SessionEvent{SessionID: "root", Event: runtime.AgentChoice("root", "root", "token")}), "streaming deltas never touch topology")
}

func TestRecoveryUncertaintyAndConnectionReachPresentation(t *testing.T) {
	a := newMetadataTestApp(t)
	a.connection.Store(uint32(ConnectionConnecting))
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"}
	sink.Reset(runtime.SessionSnapshot{Session: session.New(session.WithID("s")), Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateSettled, InterruptedTurns: 2}})
	head := a.Presentation()
	require.NotNil(t, head)
	assert.True(t, head.RecoveryUncertain(), "settled with interrupted turns is not a normal idle")
	assert.Equal(t, ConnectionConnected, head.Connection)
	sink.OnConnectionState(false, nil)
	assert.Equal(t, ConnectionConnected, a.ConnectionState(), "a gap resnapshot keeps the transport")
	sink.OnConnectionState(false, assert.AnError)
	assert.Equal(t, ConnectionReconnecting, a.Presentation().Connection)
	sink.OnConnectionState(true, nil)
	assert.Equal(t, ConnectionConnected, a.Presentation().Connection)
}

type stopTreeHandle struct {
	*projectionSession

	stops int
}

func (h *stopTreeHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, Capabilities: runtime.SessionCapabilities{StopSubtree: true}}
}

func (h *stopTreeHandle) StopSubtree(context.Context) error {
	h.stops++
	return nil
}

type stopTreeSessions struct {
	handles map[string]runtime.SessionHandle
}

func (r *stopTreeSessions) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	return r.handles[sess.ID], nil
}

func (r *stopTreeSessions) SessionByID(id string) (runtime.SessionHandle, error) {
	if h := r.handles[id]; h != nil {
		return h, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
}
func (*stopTreeSessions) DeleteSession(context.Context, string) error { return nil }

// Stop delegates to the target's owner for any session, the viewed root
// included; the owner, not the client, decides drain semantics.
func TestStopSubtreeDelegatesToTargetOwnerIncludingRoot(t *testing.T) {
	root := &stopTreeHandle{projectionSession: &projectionSession{id: "root"}}
	child := &stopTreeHandle{projectionSession: &projectionSession{id: "child"}}
	a := New(t.Context(), &stopTreeSessions{handles: map[string]runtime.SessionHandle{"root": root, "child": child}}, session.New(session.WithID("root")), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	require.True(t, a.CanStopSubtree())
	require.NoError(t, a.StopSubtree(t.Context(), SubagentTarget{SessionID: "child"}))
	require.NoError(t, a.StopSubtree(t.Context(), SubagentTarget{SessionID: "root"}))
	assert.Equal(t, 1, child.stops)
	assert.Equal(t, 1, root.stops)

	plain := New(t.Context(), &projectionSessions{session: &projectionSession{id: "plain"}}, session.New(session.WithID("plain")), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	assert.False(t, plain.CanStopSubtree())
	require.ErrorIs(t, plain.StopSubtree(t.Context(), SubagentTarget{SessionID: "child"}), runtime.ErrUnsupported)
}

func TestDelegationPolicyRequiresCanonicalCapability(t *testing.T) {
	plain := New(t.Context(), &projectionSessions{session: &projectionSession{id: "plain"}}, session.New(session.WithID("plain")), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	assert.False(t, plain.CanSetDelegationPolicy())
	_, err := plain.DelegationPolicy(t.Context())
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	require.ErrorIs(t, plain.SetDelegationPolicy(t.Context(), false), runtime.ErrUnsupported)
	assert.False(t, (*App)(nil).CanSetDelegationPolicy())
}

func TestTreeWatchCanonicalStoppedStateWinsOverRunAndInteractionOverlay(t *testing.T) {
	const rootID = "tree-owner"
	base := &subagent.Snapshot{Version: subagent.SnapshotVersion, Root: subagent.SessionRootID(rootID), Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: subagent.SessionRootID(rootID), State: subagent.NodeIdle},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Parent: subagent.SessionRootID(rootID), SessionID: "child-session", State: subagent.NodeRunning}}},
	}}}
	watch := newTreeWatch(rootID)
	watch.add(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "child-session", State: runtime.SessionStateRunning}})
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: "child-session", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child-session", RequestID: "approval"}}))
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: "child-session", Event: runtime.StreamStopped("child-session", "worker", "completed")}))
	ordinary := watch.merged(base).Nodes[0].Children[0].Node
	assert.Equal(t, subagent.NodeIdle, ordinary.State, "ending a turn is not a canonical subtree stop")
	assert.True(t, ordinary.NeedsAttention)

	stopped := *base
	stopped.Nodes = cloneNodes(base.Nodes)
	stopped.Nodes[0].Children[0].Node.State = subagent.NodeStopped
	event := runtime.SubagentTree(stopped)
	a := newMetadataTestApp(t)
	a.cancelledRequests = map[string]struct{}{"canceled-turn": {}}
	assert.Same(t, event, a.filterBridgedEvent("canceled-turn", event), "canonical tree changes survive turn cancellation")
	assert.False(t, watch.apply(runtime.SessionEvent{SessionID: "foreign-owner", Event: event}), "foreign owners cannot replace the tree")
	foreign := stopped
	foreign.Root = subagent.SessionRootID("foreign-owner")
	assert.False(t, watch.apply(runtime.SessionEvent{SessionID: rootID, Event: runtime.SubagentTree(foreign)}), "tree identity must match its owner")
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: rootID, Event: event}))
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: "child-session", Event: runtime.StreamStarted("child-session", "worker")}))
	got := watch.merged(base).Nodes[0].Children[0].Node
	assert.Equal(t, "child-session", got.SessionID)
	assert.Equal(t, subagent.NodeID("child-node"), got.ID)
	assert.Equal(t, subagent.NodeStopped, got.State, "owner snapshot wins over stale baseline and stream state")
	assert.False(t, got.NeedsAttention)
	assert.Empty(t, got.WaitingOn)
}

func TestTreeWatchOverlaysOnlyCurrentSessionBackgroundProcessCounts(t *testing.T) {
	const rootID = "background-tree-root"
	root := subagent.SessionRootID(rootID)
	base := &subagent.Snapshot{Version: subagent.SnapshotVersion, Root: root, Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: root, SessionID: rootID, State: subagent.NodeIdle},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: "child-session", Parent: root, State: subagent.NodeIdle}}},
	}}}
	watch := newTreeWatch(rootID)
	watch.reset([]runtime.SessionSnapshot{
		{Session: session.New(session.WithID(rootID)), Status: runtime.SessionStatus{SessionID: rootID}, BackgroundProcesses: []tools.BackgroundProcess{{ID: "root-proc", SessionID: rootID, State: tools.BackgroundProcessRunning}}},
		{Session: session.New(session.WithID("child-session")), Status: runtime.SessionStatus{SessionID: "child-session"}, BackgroundProcesses: []tools.BackgroundProcess{
			{ID: "child-a", SessionID: "child-session", State: tools.BackgroundProcessRunning},
			{ID: "child-b", SessionID: "child-session", State: tools.BackgroundProcessRunning},
			{ID: "foreign", SessionID: "foreign-session", State: tools.BackgroundProcessRunning},
		}},
	})
	merged := watch.merged(base)
	require.Equal(t, 1, merged.Nodes[0].Node.BackgroundProcesses)
	require.Equal(t, 2, merged.Nodes[0].Children[0].Node.BackgroundProcesses)
	require.Equal(t, subagent.NodeIdle, merged.Nodes[0].Children[0].Node.State, "background shell work does not mark a subagent's agent loop busy")
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: "child-session", Event: &runtime.BackgroundProcessesEvent{SessionID: "child-session", Processes: []tools.BackgroundProcess{{ID: "done", SessionID: "child-session", State: tools.BackgroundProcessCompleted}}}}))
	merged = watch.merged(base)
	require.Zero(t, merged.Nodes[0].Children[0].Node.BackgroundProcesses, "an authoritative terminal-only replacement clears the running badge")
	watch.add(runtime.SessionSnapshot{Session: session.New(session.WithID("child-session")), Status: runtime.SessionStatus{SessionID: "child-session"}, BackgroundProcesses: []tools.BackgroundProcess{{ID: "stale", SessionID: "child-session", State: tools.BackgroundProcessRunning}}})
	merged = watch.merged(base)
	require.Zero(t, merged.Nodes[0].Children[0].Node.BackgroundProcesses, "an older session-added snapshot cannot overwrite a newer process replacement")
	watch.reset([]runtime.SessionSnapshot{{Session: session.New(session.WithID("child-session")), Status: runtime.SessionStatus{SessionID: "child-session"}}})
	merged = watch.merged(base)
	require.Zero(t, merged.Nodes[0].Children[0].Node.BackgroundProcesses, "empty reconnect baselines clear stale counts")
	require.False(t, watch.apply(runtime.SessionEvent{SessionID: "child-session", Event: &runtime.BackgroundProcessesEvent{SessionID: "root-owner", Processes: []tools.BackgroundProcess{{ID: "foreign", SessionID: "root-owner", State: tools.BackgroundProcessRunning}}}}), "event owner and envelope must match")
}

func TestTreeWatchResolvedInteractionReplayDoesNotResurrectAttention(t *testing.T) {
	watch := newTreeWatch("root")
	root := subagent.SessionRootID("root")
	base := &subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: "child", Parent: root, State: subagent.NodeRunning}}}}}}
	watch.add(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "child", State: runtime.SessionStateRunning}})
	approval := runtime.SessionEvent{SessionID: "child", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child", RequestID: "approval"}}
	require.True(t, watch.apply(approval))
	require.Len(t, DescendantAttention(*watch.merged(base), "root"), 1)
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: "child", Event: &runtime.InteractionResolvedEvent{SessionID: "child", InteractionID: "approval"}}))
	for range 20 {
		require.False(t, watch.apply(approval), "resolved interaction replay must not become actionable again")
	}
	require.Empty(t, DescendantAttention(*watch.merged(base), "root"))
	approval.Event = &runtime.ToolCallConfirmationEvent{SessionID: "child", RequestID: "new-approval"}
	require.True(t, watch.apply(approval), "a distinct interaction still needs attention")
	require.Len(t, DescendantAttention(*watch.merged(base), "root"), 1)
}

func TestDescendantAttentionRequiresActionableReason(t *testing.T) {
	root := subagent.SessionRootID("root")
	snapshot := subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, SessionID: "root"}, Children: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "stopped", SessionID: "stopped", State: subagent.NodeStopped, NeedsAttention: true, WaitingOn: "approve tool"}},
		{Node: subagent.Node{ID: "unknown", SessionID: "unknown", State: subagent.NodeIdle, NeedsAttention: true}},
		{Node: subagent.Node{ID: "failed", SessionID: "failed", State: subagent.NodeFailed, NeedsAttention: true, Error: "model unavailable"}},
	}}}}
	items := DescendantAttention(snapshot, "root")
	require.Len(t, items, 1)
	require.Equal(t, "failed", items[0].SessionID)
	require.Equal(t, "failed", items[0].WaitingOn)
}

func TestTreeAttentionSameReasonOccurrencesSurviveReconnect(t *testing.T) {
	root := subagent.SessionRootID("root")
	base := &subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: "child", Parent: root, State: subagent.NodeRunning}}}}}}
	watch := newTreeWatch("root")
	pending := func(ids ...string) runtime.SessionSnapshot {
		snapshot := runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "child", State: runtime.SessionStateRunning}}
		for _, id := range ids {
			snapshot.Interactions = append(snapshot.Interactions, runtime.InteractionSnapshot{InteractionID: id, Kind: runtime.InteractionConfirmation})
		}
		return snapshot
	}
	watch.reset([]runtime.SessionSnapshot{pending("first")})
	first := DescendantAttention(*watch.merged(base), "root")
	require.Len(t, first, 1)
	require.Equal(t, "interaction:first", first[0].OccurrenceID)
	second := runtime.SessionEvent{SessionID: "child", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child", RequestID: "second"}}
	require.True(t, watch.apply(second))
	both := DescendantAttention(*watch.merged(base), "root")
	require.Len(t, both, 2, "same reason must not collapse distinct pending interactions")
	require.True(t, watch.apply(runtime.SessionEvent{SessionID: "child", Event: &runtime.InteractionResolvedEvent{SessionID: "child", InteractionID: "first"}}))
	remaining := DescendantAttention(*watch.merged(base), "root")
	require.Len(t, remaining, 1)
	require.Equal(t, "interaction:second", remaining[0].OccurrenceID)
	watch.reset([]runtime.SessionSnapshot{pending("first", "second")})
	require.Equal(t, remaining, DescendantAttention(*watch.merged(base), "root"), "reconnect retains live identity and resolved tombstones")
	require.False(t, watch.apply(second), "same live interaction replay is unchanged")
	require.False(t, watch.apply(runtime.SessionEvent{SessionID: "child", Event: &runtime.ToolCallConfirmationEvent{SessionID: "child", RequestID: "first"}}), "resolved replay stays nonactionable")
}
