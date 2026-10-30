package facadetest

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/tools"
)

// Tool names used by the scripted scenarios.
const (
	// ToolNeedsApproval is gated by a permission rule that asks for confirmation.
	ToolNeedsApproval = "approve_me"
	// ToolElicit asks the user a question through the runtime's elicitation handler.
	ToolElicit = "ask_user"
	// ToolNoop returns immediately.
	ToolNoop = "noop"
	// ToolSpawnSubagent is the runtime's async subagent tool; the root spawns the writer with it.
	ToolSpawnSubagent = "spawn_subagent"
	// ToolCreateTodos is the builtin todo tool that creates todo items.
	ToolCreateTodos = "create_todos"
)

// toolSet is the fixture toolset. Elicitation goes through the request-scoped
// handler the runtime installs for every tool call, as MCP toolsets do.
type toolSet struct{}

func newToolSet() *toolSet { return &toolSet{} }

func (ts *toolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{Name: ToolNeedsApproval, Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("approved-ran"), nil
		}},
		{Name: ToolNoop, Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("noop"), nil
		}},
		{Name: ToolElicit, Parameters: map[string]any{}, Handler: ts.askUser},
	}, nil
}

func (ts *toolSet) askUser(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	result, err := tools.ScopedElicitationHandler(ctx, &mcp.ElicitParams{Message: "Pick a colour", Mode: "form", RequestedSchema: map[string]any{"type": "object"}})
	if err != nil {
		return nil, err
	}
	return tools.ResultSuccess(string(result.Action)), nil
}
