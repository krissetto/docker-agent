package toolexec_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestDispatcherRejectsMismatchedResumeIdentity(t *testing.T) {
	for _, mismatch := range []string{"request", "session"} {
		t.Run(mismatch, func(t *testing.T) {
			sess := session.New()
			a := newAgent()
			var executions atomic.Int32
			tool := tools.Tool{
				Name: "shell",
				Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
					executions.Add(1)
					return tools.ResultSuccess("executed"), nil
				},
			}
			resume := make(chan toolexec.ResumeRequest, 2)
			stale := toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove, SessionID: sess.ID, RequestID: "active-call"}
			if mismatch == "request" {
				stale.RequestID = "earlier-call"
			} else {
				stale.SessionID = "unrelated-session"
			}
			resume <- stale
			resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject, SessionID: sess.ID, RequestID: "active-call", Reason: "matching request rejected"}
			dispatcher := &toolexec.Dispatcher{
				AgentFor:                func(*session.Session) *agent.Agent { return a },
				Resume:                  resume,
				RequireResponseIdentity: true,
			}
			emitter := &captureEmitter{}
			dispatcher.Process(t.Context(), sess, []tools.ToolCall{{ID: "active-call", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}}}, []tools.Tool{tool}, emitter)
			assert.Zero(t, executions.Load(), "a stale approval must never execute the active tool")
			require.Len(t, emitter.responses, 1)
			assert.True(t, emitter.responses[0].IsError)
			assert.Contains(t, emitter.responses[0].Output, "matching request rejected", "the later correctly addressed response must be consumed")
		})
	}
}
