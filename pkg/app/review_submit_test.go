package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type reviewSubmitHandle struct {
	projectionSession
	durability subagent.Durability
	agent      string
	historical bool
	executable bool
	seen       map[string]string
}

func (h *reviewSubmitHandle) AgentName() string {
	if h.agent != "" {
		return h.agent
	}
	return "root"
}
func (h *reviewSubmitHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: h.AgentName(), Historical: h.historical, ExecutionAvailable: h.executable, Capabilities: runtime.SessionCapabilities{Durability: h.durability}}
}

func (h *reviewSubmitHandle) Submit(ctx context.Context, in runtime.TurnInput) (runtime.Submission, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Submission{}, err
	}
	if h.seen == nil {
		h.seen = map[string]string{}
	}
	if turn, ok := h.seen[in.RequestID]; ok {
		return runtime.Submission{SessionID: h.id, TurnID: turn}, nil
	}
	h.next++
	turn := fmt.Sprint(h.next)
	h.seen[in.RequestID] = turn
	h.inputs = append(h.inputs, in)
	return runtime.Submission{SessionID: h.id, TurnID: turn}, nil
}

type reviewSubmitRuntime struct{ handle runtime.SessionHandle }

func (r *reviewSubmitRuntime) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return r.handle, nil
}
func (r *reviewSubmitRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, Operation: "lookup"}
}
func (*reviewSubmitRuntime) DeleteSession(context.Context, string) error { return nil }

func TestSubmitReviewInputUsesLiteralCanonicalAdmissionAndStableRetryID(t *testing.T) {
	sess := session.New(session.WithID("review-target"), session.WithAgentName("root"), session.WithWorkingDir("/repo"))
	handle := &reviewSubmitHandle{projectionSession: projectionSession{id: sess.ID}, durability: subagent.DurabilityDurable, executable: true}
	services := &reviewSubmitRuntime{handle: handle}
	a := New(t.Context(), services, sess, runtime.SessionBinding{AgentName: "root", Durability: subagent.DurabilityDurable})
	target := a.CaptureReviewSubmitTarget()
	receipt, err := a.SubmitReviewInput(t.Context(), target, "review:stable", "literal /changes ! not a shell command")
	require.NoError(t, err)
	require.True(t, receipt.Durable)
	require.Equal(t, sess.ID, receipt.Submission.SessionID)
	require.Equal(t, "literal /changes ! not a shell command", handle.inputs[0].Content)
	require.Equal(t, "review:stable", handle.inputs[0].RequestID)
	retry, err := a.SubmitReviewInput(t.Context(), target, "review:stable", "literal /changes ! not a shell command")
	require.NoError(t, err)
	require.Equal(t, receipt.Submission.TurnID, retry.Submission.TurnID)
	require.Len(t, handle.inputs, 1, "same request ID is deduplicated at the session admission boundary")
}

func TestSubmitReviewInputRejectsChangedTargetAndReadOnly(t *testing.T) {
	sess := session.New(session.WithID("review-target"), session.WithAgentName("root"), session.WithWorkingDir("/repo"))
	first := &projectionSession{id: sess.ID}
	a := New(t.Context(), &reviewSubmitRuntime{handle: first}, sess, runtime.SessionBinding{AgentName: "root"})
	target := a.CaptureReviewSubmitTarget()
	second := &reviewSubmitHandle{projectionSession: projectionSession{id: "replacement"}}
	a.replaceSessionState(sessionState{session: session.New(session.WithID("replacement")), handle: second, binding: runtime.SessionBinding{AgentName: "root"}})
	_, err := a.SubmitReviewInput(t.Context(), target, "id", "body")
	require.ErrorIs(t, err, ErrReviewSubmitTargetChanged)
	require.Empty(t, first.inputs)
	readOnly := New(t.Context(), nil, session.New(session.WithID("readonly")), runtime.SessionBinding{}, WithReadOnly())
	_, err = readOnly.SubmitReviewInput(t.Context(), ReviewSubmitTarget{}, "id", "body")
	require.Error(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = a.SubmitReviewInput(canceled, ReviewSubmitTarget{}, "id", "body")
	require.ErrorIs(t, err, context.Canceled)
}

func TestSubmitReviewInputValidatesAttachedIdentity(t *testing.T) {
	parent := session.New(session.WithID("parent"), session.WithAgentName("root"), session.WithWorkingDir("/repo"))
	child := session.New(session.WithID("child"), session.WithAgentName("worker"), session.WithWorkingDir("/repo"))
	handle := &reviewSubmitHandle{projectionSession: projectionSession{id: "child"}, durability: subagent.DurabilityVolatile, agent: "worker", executable: true}
	a, err := NewResolved(t.Context(), &reviewSubmitRuntime{handle: handle}, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: "child", RootSessionID: "parent", Session: child, WorkingDir: "/repo", Binding: runtime.SessionBinding{AgentName: "worker", ParentSessionID: "parent"}, Attach: &runtime.SubagentAttachInfo{NodeID: "node", ParentSessionID: "parent", Agent: "worker", Session: child}}})
	require.NoError(t, err)
	target := a.CaptureReviewSubmitTarget()
	require.Equal(t, subagent.NodeID("node"), target.AttachedNodeID)
	receipt, err := a.SubmitReviewInput(t.Context(), target, "review:child", "comment body")
	require.NoError(t, err)
	require.Equal(t, "child", receipt.Submission.SessionID)
	require.Equal(t, "comment body", handle.inputs[0].Content)
	target.AttachedNodeID = "other-node"
	_, err = a.SubmitReviewInput(t.Context(), target, "review:other", "must not send")
	require.ErrorIs(t, err, ErrReviewSubmitTargetChanged)
	require.Len(t, handle.inputs, 1)
	_ = parent
}
