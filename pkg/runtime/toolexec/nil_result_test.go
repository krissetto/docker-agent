package toolexec_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNilToolResultBecomesRecordedToolError(t *testing.T) {
	a := newAgent()
	sess := session.New(session.WithToolsApproved(true))
	dispatcher := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }}
	emitter := &captureEmitter{}
	dispatcher.Process(t.Context(), sess, []tools.ToolCall{{ID: "nil", Function: tools.FunctionCall{Name: "nil", Arguments: "{}"}}}, []tools.Tool{{Name: "nil", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) { return nil, nil }}}, emitter)
	require.Len(t, emitter.responses, 1)
	require.True(t, emitter.responses[0].IsError)
	require.Contains(t, emitter.responses[0].Output, "nil result")
	require.Len(t, sess.Messages, 1)
	require.Contains(t, sess.Messages[0].Message.Message.Content, "nil result")
}
