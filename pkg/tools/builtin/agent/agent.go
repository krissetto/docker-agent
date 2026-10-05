package agent

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameRunBackgroundAgent   = "run_background_agent"
	ToolNameListBackgroundAgents = "list_background_agents"
	ToolNameViewBackgroundAgent  = "view_background_agent"
	ToolNameStopBackgroundAgent  = "stop_background_agent"
)

// CreateToolSet is used by the tools registry.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

// RunBackgroundAgentArgs specifies the parameters for dispatching a sub-agent task asynchronously.
type RunBackgroundAgentArgs struct {
	Agent          string `json:"agent" jsonschema:"The name of the sub-agent to run in the background."`
	Task           string `json:"task" jsonschema:"A clear and concise description of the task the agent should achieve."`
	ExpectedOutput string `json:"expected_output,omitempty" jsonschema:"The expected output from the agent (optional)."`
}

// ViewBackgroundAgentArgs specifies the task ID to inspect.
type ViewBackgroundAgentArgs struct {
	TaskID string `json:"task_id" jsonschema:"The ID of the background agent task to view."`
}

// StopBackgroundAgentArgs specifies the task ID to cancel.
type StopBackgroundAgentArgs struct {
	TaskID string `json:"task_id" jsonschema:"The ID of the background agent task to stop."`
}

// RunParams holds the parameters for running a sub-agent.
type RunParams struct {
	AgentName      string
	Task           string
	ExpectedOutput string
	ParentSession  *session.Session
	OnContent      func(content string)
}

// RunResult holds the outcome of a sub-agent execution.
type RunResult struct {
	Result string // final assistant message on completion
	ErrMsg string // error detail if failed
}

// New returns a lightweight ToolSet for registering background agent
// tool definitions and instructions. It does not require a Runner and is
// suitable for use in the teamloader registry.
func New() tools.ToolSet {
	return &ToolSet{}
}

// ToolSet provides tool definitions and instructions without a Runner.
type ToolSet struct{}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return backgroundAgentTools(), nil
}

func (t *ToolSet) Instructions() string {
	return `# Background Agent Tasks

Use background agent tasks to dispatch work to sub-agents concurrently.

- **run_background_agent**: Start a command and return its task ID. Native sub-agents inherit the current session's safety policy and permissions; calls requiring confirmation are denied because background tasks are non-interactive. External harnesses enforce their own permission model.
- **list_background_agents**: Show all tasks with status and runtime
- **view_background_agent**: Get output and status of a task by task_id
- **stop_background_agent**: Terminate a task by task_id

**Notes**: Tasks use persistent child sessions and the same root resource policy as other delegations. Lists and controls are scoped to the calling session tree.`
}

func backgroundAgentTools() []tools.Tool {
	return []tools.Tool{
		{
			Name:     ToolNameRunBackgroundAgent,
			Category: "transfer",
			Description: `Start a sub-agent task in the background and return immediately with a task ID.
Use this to dispatch work to multiple sub-agents concurrently. Native sub-agents inherit the current
session's safety policy and permissions; calls requiring confirmation are denied because background
tasks are non-interactive. External harnesses enforce their own permission model. Check progress with
view_background_agent and collect results once the task is complete.`,
			Parameters:  tools.MustSchemaFor[RunBackgroundAgentArgs](),
			Annotations: tools.ToolAnnotations{Title: "Run Background Agent"},
		},
		{
			Name:        ToolNameListBackgroundAgents,
			Category:    "transfer",
			Description: `List all background agent tasks with their status and runtime.`,
			Annotations: tools.ToolAnnotations{
				Title:        "List Background Agents",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameViewBackgroundAgent,
			Category:    "transfer",
			Description: `View the output and status of a specific background agent task by task ID. Returns live buffered output if still running, or the final result if complete.`,
			Parameters:  tools.MustSchemaFor[ViewBackgroundAgentArgs](),
			Annotations: tools.ToolAnnotations{
				Title:        "View Background Agent",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameStopBackgroundAgent,
			Category:    "transfer",
			Description: `Stop a running background agent task by task ID.`,
			Parameters:  tools.MustSchemaFor[StopBackgroundAgentArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Stop Background Agent",
			},
		},
	}
}
