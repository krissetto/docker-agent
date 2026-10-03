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
	assert.Equal(t, []TreeAttention{{SubagentTarget: grand, WaitingOn: waitingOnAnswer}}, DescendantAttention(snapshot, "child-session"), "a nested view aggregates only its own subtree")

	tree <- runtime.SessionEvent{SessionID: "child-session", Event: &runtime.InteractionResolvedEvent{SessionID: "child-session", InteractionID: "approval"}}
	snapshot = next(func(s subagent.Snapshot) bool { return len(DescendantAttention(s, root.ID)) == 1 })
	assert.Equal(t, "grand-session", DescendantAttention(snapshot, root.ID)[0].SessionID)
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
