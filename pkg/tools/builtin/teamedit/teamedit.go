package teamedit

import (
	"context"
	"encoding/json"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolsetType = "team_edit"
const ToolName = "team_edit"
const PersistToolName = "team_edit_persist"
const ExportToolName = "team_edit_export"

// Call carries inspect/apply/replace requests through the runtime handler.
type Call struct {
	Action            string    `json:"action" jsonschema:"Operation to perform: inspect, apply, or replace"`
	Revision          string    `json:"revision,omitempty" jsonschema:"Current source revision token from inspect"`
	AgentKey          string    `json:"agent_key,omitempty" jsonschema:"Target authored agent key for apply"`
	CloneFrom         string    `json:"clone_from,omitempty" jsonschema:"Existing authored agent key to clone"`
	NewAgentKey       string    `json:"new_agent_key,omitempty" jsonschema:"New authored key for a cloned agent"`
	RenameTo          string    `json:"rename_to,omitempty" jsonschema:"New authored key for rename"`
	Instruction       *string   `json:"instruction,omitempty" jsonschema:"Replace target instruction"`
	Description       *string   `json:"description,omitempty" jsonschema:"Replace target description"`
	WelcomeMessage    *string   `json:"welcome_message,omitempty" jsonschema:"Replace target welcome message"`
	Model             *string   `json:"model,omitempty" jsonschema:"Select a model in this agent's baseline allowlist"`
	FallbackModels    *[]string `json:"fallback_models,omitempty" jsonschema:"Replace fallbacks with baseline-allowed models"`
	ModelPickerModels *[]string `json:"model_picker_models,omitempty" jsonschema:"Replace model-picker options with baseline-allowed models"`
	Remove            bool      `json:"remove,omitempty" jsonschema:"Remove the target agent when permitted"`
	YAML              string    `json:"yaml,omitempty" jsonschema:"Complete YAML document for full-authority replacement"`
	DryRun            bool      `json:"dry_run,omitempty" jsonschema:"Validate without publishing the replacement"`
	Path              string    `json:"path,omitempty" jsonschema:"Destination path inside the trusted export root"`
}

type PersistCall struct {
	Revision string `json:"revision" jsonschema:"Current source revision token from inspect"`
}

type ExportCall struct {
	Revision string `json:"revision" jsonschema:"Source revision token to export"`
	Path     string `json:"path" jsonschema:"New destination path inside the trusted export root"`
}

type ToolSet struct{ Handler tools.ToolHandler }

func Definitions(handler tools.ToolHandler) []tools.Tool {
	return []tools.Tool{
		{Name: ToolName, Category: "team", Description: "Inspect or make a controlled edit to this session's team source", Parameters: tools.MustSchemaFor[Call](), OutputSchema: tools.MustSchemaFor[string](), Handler: handler},
		{Name: PersistToolName, Category: "team", Description: "Persist the current controlled team revision to its trusted source", Parameters: tools.MustSchemaFor[PersistCall](), OutputSchema: tools.MustSchemaFor[string](), Handler: withAction(handler, "persist")},
		{Name: ExportToolName, Category: "team", Description: "Export a team revision to a new file under the trusted export root", Parameters: tools.MustSchemaFor[ExportCall](), OutputSchema: tools.MustSchemaFor[string](), Handler: withAction(handler, "export")},
	}
}

func withAction(handler tools.ToolHandler, action string) tools.ToolHandler {
	return func(ctx context.Context, call tools.ToolCall, runtime tools.Runtime) (*tools.ToolCallResult, error) {
		arguments := map[string]any{}
		if call.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
				return nil, err
			}
		}
		arguments["action"] = action
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, err
		}
		call.Function.Arguments = string(encoded)
		return handler(ctx, call, runtime)
	}
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if t == nil || t.Handler == nil {
		return nil, nil
	}
	return Definitions(t.Handler), nil
}
