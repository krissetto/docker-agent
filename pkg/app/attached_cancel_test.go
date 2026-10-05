package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestAttachedCancelUsesCanonicalBaseline(t *testing.T) {
	sess := session.New(session.WithID("review-active-attach"), session.WithAgentName("root"))
	handle := &projectionSession{id: sess.ID, activeTurnID: "server-owned-turn"}
	a := New(t.Context(), &projectionSessions{session: handle}, sess, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: sess.ID}
	sink.Reset(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, AgentName: "root", State: runtime.SessionStateRunning, TurnID: "server-owned-turn"}})
	sink.Apply(runtime.SessionEvent{Version: 2, SessionID: sess.ID, TranscriptPosition: -1, Event: runtime.StreamStarted(sess.ID, "root")})
	require.Equal(t, "server-owned-turn", a.Presentation().Status.TurnID)
	outcome := a.CancelRun()
	require.Equal(t, runtime.CancelAccepted, outcome)
	require.Equal(t, "server-owned-turn", handle.cancelTurnID)
	t.Logf("canonical active turn=%q, App cancellation=%q, handle.Cancel called=%v", a.Presentation().Status.TurnID, outcome, handle.cancelTurnID != "")
}

func TestCancelResetReplacesHistoricSubmissionTarget(t *testing.T) {
	sess := session.New(session.WithID("review-resnapshot"), session.WithAgentName("root"))
	h := &projectionSession{id: sess.ID, activeTurnID: "new-server-turn"}
	a := New(t.Context(), &projectionSessions{session: h}, sess, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	a.recordSubmission("settled-old-turn")
	sink := &appProjectionSink{app: a, ctx: context.WithoutCancel(t.Context()), sessionID: sess.ID}
	sink.Reset(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: "new-server-turn"}})
	require.Equal(t, runtime.CancelAccepted, a.CancelRun())
	require.Equal(t, "new-server-turn", h.cancelTurnID)
	t.Logf("canonical turn=new-server-turn, actual Cancel argument=%q", h.cancelTurnID)
}

type reviewAttachBlockingProvider struct {
	stubProvider

	started chan struct{}
}

func (p reviewAttachBlockingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	close(p.started)
	return &reviewAttachBlockingStream{ctx: ctx}, nil
}

type reviewAttachBlockingStream struct {
	ctx context.Context //nolint:containedctx // stream ends with the canonical provider invocation
}

func (s *reviewAttachBlockingStream) Recv() (chat.MessageStreamResponse, error) {
	<-s.ctx.Done()
	return chat.MessageStreamResponse{}, s.ctx.Err()
}
func (*reviewAttachBlockingStream) Close() {}
func TestRealCanonicalAttachedAppCancelsRunningTurn(t *testing.T) {
	p := reviewAttachBlockingProvider{started: make(chan struct{})}
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(p)))))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sess := session.New(session.WithAgentName("root"))
	h, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	accepted, err := h.Submit(t.Context(), runtime.TurnInput{Content: "blocking fake provider"})
	require.NoError(t, err)
	<-p.started
	snap, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	a := New(t.Context(), owner.Runtime(), snap, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(rt))
	t.Cleanup(a.Close)
	require.True(t, a.startSessionEventBridge(t.Context()))
	require.Eventually(t, func() bool { return a.Presentation() != nil && a.Presentation().Status.TurnID == accepted.TurnID }, time.Second, time.Millisecond)
	require.Equal(t, runtime.CancelAccepted, a.CancelRun())
	require.NoError(t, h.AwaitTurn(t.Context(), accepted.TurnID))
}

func TestCapturedCancelCannotRetargetReplacement(t *testing.T) {
	sess := session.New(session.WithID("intent"), session.WithAgentName("root"))
	h := &projectionSession{id: sess.ID, activeTurnID: "old"}
	a := New(t.Context(), &projectionSessions{session: h}, sess, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: sess.ID}
	reset := func(id string) {
		sink.Reset(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: id}})
	}
	reset("old")
	intent := a.CaptureCancelRun()
	reset("new")
	h.activeTurnID = "new"
	require.Equal(t, runtime.CancelNotActive, intent())
	require.Empty(t, h.cancelTurnID)
	require.Equal(t, runtime.CancelAccepted, a.CancelRun())
	require.Equal(t, "new", h.cancelTurnID)
	intent = a.CaptureCancelRun()
	a.replaceSessionState(sessionState{session: sess, handle: h})
	require.Equal(t, runtime.CancelNotActive, a.CancelRun())
	reset("new")
	require.Equal(t, runtime.CancelNotActive, intent(), "replacement invalidates even the same handle and turn identity")
}
