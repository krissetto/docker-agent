package teamedit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCreatorRequiresExternalGrant(t *testing.T) {
	t.Parallel()
	off, err := Creator(t.Context(), latest.Toolset{Type: ToolsetType}, "", &config.RuntimeConfig{}, "root")
	require.NoError(t, err)
	calls, err := off.Tools(t.Context())
	require.NoError(t, err)
	require.Empty(t, calls)
	on, err := Creator(t.Context(), latest.Toolset{Type: ToolsetType}, "", &config.RuntimeConfig{Config: config.Config{TeamEditMode: "restricted"}}, "root")
	require.NoError(t, err)
	calls, err = on.Tools(t.Context())
	require.NoError(t, err)
	require.Empty(t, calls)
}

func TestToolsetExposesNoPlaceholderToolsBeforeRuntimeIntegration(t *testing.T) {
	t.Parallel()
	tools, err := (&ToolSet{}).Tools(t.Context())
	require.NoError(t, err)
	require.Empty(t, tools)
}

func TestPersistAndExportToolsHaveSeparateSchemasAndDispatchActions(t *testing.T) {
	t.Parallel()
	var calls []tools.ToolCall
	handler := func(_ context.Context, call tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		calls = append(calls, call)
		return tools.ResultSuccess("ok"), nil
	}
	definitions := Definitions(handler)
	require.Len(t, definitions, 3)
	require.Equal(t, ToolName, definitions[0].Name)
	require.Equal(t, PersistToolName, definitions[1].Name)
	require.Equal(t, ExportToolName, definitions[2].Name)

	_, err := definitions[1].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: PersistToolName, Arguments: `{"revision":"r1"}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	_, err = definitions[2].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: ExportToolName, Arguments: `{"revision":"r1","path":"export.yaml"}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	require.Contains(t, calls[0].Function.Arguments, `"action":"persist"`)
	require.Contains(t, calls[1].Function.Arguments, `"action":"export"`)
	require.Contains(t, calls[1].Function.Arguments, `"path":"export.yaml"`)
}

func TestCallSchemaSupportsEveryAgentPatchOperation(t *testing.T) {
	t.Parallel()
	input := `{"action":"apply","revision":"r1","clone_from":"worker","new_agent_key":"worker_copy","agent_key":"worker_copy","rename_to":"renamed","instruction":"updated","fallback_models":["openai/a"],"model_picker_models":["openai/b"],"remove":false}`
	var call Call
	require.NoError(t, json.Unmarshal([]byte(input), &call))
	require.Equal(t, "worker", call.CloneFrom)
	require.Equal(t, "worker_copy", call.NewAgentKey)
	require.NotNil(t, call.FallbackModels)
	require.Equal(t, []string{"openai/a"}, *call.FallbackModels)
	require.NotNil(t, call.ModelPickerModels)
	require.Equal(t, []string{"openai/b"}, *call.ModelPickerModels)
}
