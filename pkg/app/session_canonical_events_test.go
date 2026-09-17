package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestCanonicalProjectionPreservesOwnerTurnSequenceAndEpoch(t *testing.T) {
	for _, event := range []runtime.Event{
		&runtime.TurnSettledEvent{Type: "turn_settled", SessionID: "actual", TurnID: "accepted", Outcome: runtime.TurnCompleted},
		&runtime.TurnSettledEvent{Type: "turn_settled", SessionID: "actual", TurnID: "accepted", Outcome: runtime.TurnCanceled},
		&runtime.TurnSettledEvent{Type: "turn_settled", SessionID: "actual", TurnID: "accepted", Outcome: runtime.TurnFailed},
		&runtime.SubagentCreatedEvent{Type: "subagent_created", SessionID: "actual", ParentSessionID: "actual", ChildSessionID: "child", NodeID: "node", CreatedAt: time.Unix(123, 0)},
	} {
		a := newMetadataTestApp(t)
		a.bridgeEpoch.Store(4)
		a.cancelledRequests = map[string]struct{}{"accepted": {}}
		sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "attachment", epoch: 4}
		sink.Apply(runtime.SessionEvent{SessionID: "actual", TurnID: "accepted", Sequence: 9, Event: event})
		select {
		case value := <-a.events:
			message, ok := value.(SessionEventMsg)
			require.True(t, ok)
			assert.Same(t, event, message.Event)
			assert.Equal(t, "actual", message.OriginSessionID)
			assert.Equal(t, "accepted", message.TurnID)
			assert.Equal(t, uint64(9), message.Sequence)
			assert.Equal(t, uint64(4), message.Epoch)
			assert.False(t, message.Seed)
		case <-time.After(time.Second):
			t.Fatal("canonical event was filtered as canceled presentation output")
		}
		a.bridgeEpoch.Store(5)
		sink.Apply(runtime.SessionEvent{SessionID: "actual", TurnID: "accepted", Sequence: 10, Event: event})
		assert.Empty(t, a.events, "stale attachment cannot forward canonical events")
	}
}

func TestCanonicalProjectionSnapshotIsSeedNotCompletion(t *testing.T) {
	a := newMetadataTestApp(t)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "owner"}
	sink.Reset(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "owner", State: runtime.SessionStateSettled, TurnID: "historical"}, Cursor: 7})
	message := (<-a.events).(SessionEventMsg)
	assert.True(t, message.Seed)
	assert.Zero(t, message.Sequence)
	assert.Empty(t, message.TurnID)
	_, reset := message.Event.(*SessionResetEvent)
	assert.True(t, reset)
	event := &runtime.TurnSettledEvent{SessionID: "owner", TurnID: "new", Outcome: runtime.TurnCompleted}
	sink.Apply(runtime.SessionEvent{TurnID: "new", Sequence: 8, Event: event})
	message = (<-a.events).(SessionEventMsg)
	assert.Equal(t, "owner", message.OriginSessionID, "legacy envelope owner fallback remains compatible")
	assert.Equal(t, uint64(8), message.Sequence)
	assert.False(t, message.Seed)
}

func TestCanonicalCurrentSessionEventRejectsFirstBufferedOldAttachment(t *testing.T) {
	a := newMetadataTestApp(t)
	a.replaceSessionState(sessionState{session: session.New(session.WithID("same"))})
	a.bridgeEpoch.Store(4)
	buffered := SessionEventMsg{
		Event:           &runtime.TurnSettledEvent{SessionID: "same", TurnID: "accepted", Outcome: runtime.TurnCompleted},
		OriginSessionID: "same", TurnID: "accepted", Epoch: 4, Sequence: 9,
	}
	require.True(t, a.IsCurrentSessionEvent(buffered))
	// Model the bridge replacement boundary before the consumer has observed
	// any event from the new attachment: observed-epoch highwater is insufficient.
	a.bridgeEpoch.Store(5)
	a.replaceSessionState(sessionState{session: session.New(session.WithID("same"))})
	assert.False(t, a.IsCurrentSessionEvent(buffered))
	current := buffered
	current.Epoch = 5
	assert.True(t, a.IsCurrentSessionEvent(current))
	current.OriginSessionID = "other"
	assert.False(t, a.IsCurrentSessionEvent(current))
	current.Epoch = 0
	assert.True(t, a.IsCurrentSessionEvent(current), "zero epoch retains legacy/synthetic compatibility")
	assert.Equal(t, uint64(5), a.bridgeEpoch.Load(), "validation is read-only")
	assert.Equal(t, "same", a.Session().ID)
}

func TestCanonicalCurrentSessionEventWithoutInstalledSession(t *testing.T) {
	a := newMetadataTestApp(t)
	a.bridgeEpoch.Store(4)
	assert.True(t, a.IsCurrentSessionEvent(SessionEventMsg{}))
	assert.True(t, a.IsCurrentSessionEvent(SessionEventMsg{Epoch: 4, OriginSessionID: "owner"}))
	assert.False(t, a.IsCurrentSessionEvent(SessionEventMsg{Epoch: 3, OriginSessionID: "owner"}))
}

func TestCanonicalCapturedJobIdentityIsReadonlyAndFencesSameSessionReattach(t *testing.T) {
	a := newMetadataTestApp(t)
	a.replaceSessionState(sessionState{session: session.New(session.WithID("same"))})
	a.bridgeEpoch.Store(4)
	identity := a.CurrentSessionEventIdentity()
	assert.Equal(t, SessionEventMsg{Epoch: 4, OriginSessionID: "same"}, identity)
	require.True(t, a.IsCurrentSessionEvent(identity))
	assert.Equal(t, uint64(4), a.bridgeEpoch.Load())
	assert.Equal(t, "same", a.Session().ID)
	assert.Empty(t, a.events)
	// A result from an already launched job cannot be relabeled with the new
	// attachment's identity, even when the replacement has the same session ID.
	a.bridgeEpoch.Store(5)
	a.replaceSessionState(sessionState{session: session.New(session.WithID("same"))})
	assert.False(t, a.IsCurrentSessionEvent(identity))
	current := a.CurrentSessionEventIdentity()
	assert.Equal(t, SessionEventMsg{Epoch: 5, OriginSessionID: "same"}, current)
	assert.True(t, a.IsCurrentSessionEvent(current))
	a.replaceSessionState(sessionState{})
	assert.Equal(t, SessionEventMsg{Epoch: 5}, a.CurrentSessionEventIdentity())
}

// This handle fails if construction attempts model reconciliation or hydration.
type resolvedOnlyHandle struct{ *projectionSession }

func (*resolvedOnlyHandle) Hydrate(context.Context) error { panic("resolved constructor hydrated") }
func (*resolvedOnlyHandle) SetModel(context.Context, string) error {
	panic("resolved constructor changed model")
}

func (*resolvedOnlyHandle) Metadata() runtime.SessionMetadata {
	panic("resolved constructor fetched metadata")
}

func TestCanonicalNewResolvedNeverReconcilesCommittedOwner(t *testing.T) {
	handle := &resolvedOnlyHandle{projectionSession: &projectionSession{id: "committed"}}
	sess := session.New(session.WithID(handle.ID()), session.WithAgentName("root"))
	sess.WorkingDir = "/synthetic/workspace"
	committed := runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, RootSessionID: sess.ID, Session: sess, Binding: runtime.SessionBinding{AgentName: "root", Model: "provider/model"}, WorkingDir: sess.WorkingDir}}
	a, err := NewResolved(t.Context(), nil, committed, WithRuntimeServices(nil))
	require.NoError(t, err)
	assert.Same(t, handle, a.SessionHandle())
	assert.Equal(t, sess.ID, a.Session().ID)
	sess.SetTitle("external mutation")
	assert.NotEqual(t, "external mutation", a.Session().TitleSnapshot())
	committed.Info.SessionID = "foreign"
	invalid, err := NewResolved(t.Context(), nil, committed)
	require.Error(t, err)
	assert.Nil(t, invalid)
}

func TestCanonicalDormancyProjectionUsesSnapshotAndEvent(t *testing.T) {
	a := newMetadataTestApp(t)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "owner"}
	sink.Reset(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "owner", Dormant: true, Pending: 2}})
	assert.True(t, a.Presentation().Status.Dormant)
	sink.Apply(runtime.SessionEvent{SessionID: "owner", Sequence: 1, Event: &runtime.DormancyChangedEvent{SessionID: "owner", Dormant: false}})
	assert.False(t, a.Presentation().Status.Dormant)
}

func TestCanonicalNewResolvedStartDoesNotRestoreOrWakeArchivedTree(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker", runtime.SessionParentAgentAttribute: "root"}))
	for _, sess := range []*session.Session{root, child} {
		input := session.UserMessage("archived")
		input.Pending, input.Accepted, input.TurnID = true, true, sess.ID+"-pending"
		sess.AddMessage(input)
		require.NoError(t, store.AddSession(t.Context(), sess))
	}
	topology := subagent.NewInMemoryStore()
	rootNode := subagent.SessionRootID(root.ID)
	require.NoError(t, topology.SaveTree(t.Context(), root.ID, subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Parent: rootNode, SessionID: child.ID, Agent: "worker", State: subagent.NodeIdle}}}}}}))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(stubProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(stubProvider{})),
	)), runtime.WithSessionStore(store), runtime.WithSubagentStore(topology))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	prepared, err := owner.Runtime().(runtime.SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	require.NotNil(t, committed.Info.Session.GetSubagentTree())
	a, err := NewResolved(t.Context(), owner.Runtime(), committed, WithRuntimeServices(&mockRuntime{}))
	require.NoError(t, err)
	a.Start(t.Context())
	for _, id := range []string{root.ID, child.ID} {
		handle, err := owner.Runtime().SessionByID(id)
		require.NoError(t, err)
		status, err := handle.Status(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Dormant)
		assert.Equal(t, 1, status.Pending)
		assert.NotEqual(t, runtime.SessionStateRunning, status.State)
	}
}
