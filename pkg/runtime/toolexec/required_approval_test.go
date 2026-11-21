package toolexec_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRequiredApprovalOnlyTightensAndDoesNotPersist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision toolexec.ResumeType
		deny     bool
	}{
		{"yolo and allowing hooks", toolexec.ResumeTypeApprove, false},
		{"remember tool remains once", toolexec.ResumeTypeApproveTool, false},
		{"autonomous remains once", toolexec.ResumeTypeApproveAutonomous, false},
		{"balanced remains once", toolexec.ResumeTypeApproveBalanced, false},
		{"reject", toolexec.ResumeTypeReject, false},
		{"policy denial cannot be approved", toolexec.ResumeTypeApprove, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hd, rec := newPhaseHooks(t, &hooks.Config{PreToolUse: matchAll(hook(bVerdict, "allow", "allowed")), PermissionRequest: matchAll(hook(bPermit))})
			sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
			sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell"}}
			if tc.deny {
				sess.Permissions = &session.PermissionsConfig{Deny: []string{"shell"}}
			}
			resume := make(chan toolexec.ResumeRequest, 1)
			resume <- toolexec.ResumeRequest{Type: tc.decision}
			d := newDispatcher(hd, resume)
			d.Permissions = sessionCheckers
			var got []string
			em := &captureEmitter{}
			d.Process(toolexec.WithRequiredApproval(t.Context()), sess, []tools.ToolCall{shellCall("x", "mkdir out")}, []tools.Tool{shellTool(&got)}, em)
			if tc.deny {
				require.Empty(t, em.confirmations)
			} else {
				require.Len(t, em.confirmations, 1)
			}
			if tc.deny || tc.decision == toolexec.ResumeTypeReject {
				require.Empty(t, got)
			} else {
				require.Len(t, got, 1)
			}
			require.Zero(t, rec.count(hooks.EventPermissionRequest), "hooks cannot silently approve constrained input")
			require.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy())
			if tc.deny {
				require.Equal(t, []string{"shell"}, sess.Permissions.Deny)
			} else {
				require.Equal(t, []string{"shell"}, sess.Permissions.Allow)
			}
			// A later unconstrained TUI turn retains its ordinary autonomous policy.
			if !tc.deny {
				next := &captureEmitter{}
				d.Process(t.Context(), sess, []tools.ToolCall{shellCall("next", "mkdir out")}, []tools.Tool{shellTool(&got)}, next)
				require.Empty(t, next.confirmations)
			}
		})
	}
}

func TestRequiredApprovalRetainedForNestedToolContinuation(t *testing.T) {
	inner := tools.Tool{Name: "inner", Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess("effect"), nil
	}}
	outer := tools.Tool{Name: "outer", Handler: func(ctx context.Context, _ tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
		return rt.(interface {
			InvokeTool(context.Context, tools.Tool, tools.ToolCall) (*tools.ToolCallResult, error)
		}).InvokeTool(ctx, inner, tools.ToolCall{Function: tools.FunctionCall{Name: "inner", Arguments: "{}"}})
	}}
	resume := make(chan toolexec.ResumeRequest, 2)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject}
	d := newDispatcher(nil, resume)
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	d.Process(toolexec.WithRequiredApproval(t.Context()), sess, []tools.ToolCall{{ID: "outer", Function: tools.FunctionCall{Name: "outer", Arguments: "{}"}}}, []tools.Tool{outer}, em)
	require.Len(t, em.confirmations, 2, "approval of aggregate does not approve nested effects")
	require.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy())
}

func TestRequiredApprovalCancellationDoesNotLeak(t *testing.T) {
	ctx, cancel := context.WithCancel(toolexec.WithRequiredApproval(t.Context()))
	confirmations := make(chan struct{})
	em := &captureEmitter{confirmed: confirmations}
	d := newDispatcher(nil, nil)
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Process(ctx, sess, []tools.ToolCall{shellCall("cancelled", "mkdir out")}, []tools.Tool{shellTool(&got)}, em)
	}()
	select {
	case <-confirmations:
	case <-time.After(time.Second):
		t.Fatal("missing confirmation")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not settle confirmation")
	}
	require.Empty(t, got)
	next := &captureEmitter{}
	d.Process(t.Context(), sess, []tools.ToolCall{shellCall("ordinary", "mkdir out")}, []tools.Tool{shellTool(&got)}, next)
	require.Empty(t, next.confirmations)
	require.Len(t, got, 1)
}
