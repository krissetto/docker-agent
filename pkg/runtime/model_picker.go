package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
)

// findModelPickerTool returns the Tool from the current agent's
// toolsets, or nil if the agent has no model_picker configured.
func (r *LocalRuntime) findModelPickerTool() *modelpicker.ToolSet {
	currentName := r.currentAgentName()
	a, err := r.team.Agent(currentName)
	if err != nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if mpt, ok := tools.As[*modelpicker.ToolSet](ts); ok {
			return mpt
		}
	}
	return nil
}

// handleChangeModel handles the change_model tool call by switching the
// model of the session the call runs in.
func (r *LocalRuntime) handleChangeModel(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var params modelpicker.ChangeModelArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Model == "" {
		return tools.ResultError("model parameter is required"), nil
	}

	// Validate the requested model against the allowed list
	mpt := r.findModelPickerTool()
	if mpt == nil {
		return tools.ResultError("model_picker is not configured for this agent"), nil
	}
	allowed := mpt.AllowedModels()
	if !slices.Contains(allowed, params.Model) {
		return tools.ResultError(fmt.Sprintf(
			"model %q is not in the allowed list. Available models: %s",
			params.Model, strings.Join(allowed, ", "),
		)), nil
	}

	return r.setModelAndEmitInfo(ctx, sess, params.Model, events)
}

// handleRevertModel handles the revert_model tool call by reverting the
// session's agent to its default model.
func (r *LocalRuntime) handleRevertModel(ctx context.Context, sess *session.Session, _ tools.ToolCall, events EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	return r.setModelAndEmitInfo(ctx, sess, "", events)
}

// setModelAndEmitInfo changes the model of the session's session — the same
// per-session, persisted override /model applies — and emits an updated
// AgentInfo event so the UI reflects the change. An empty modelRef reverts to
// the agent's default model.
func (r *LocalRuntime) setModelAndEmitInfo(ctx context.Context, sess *session.Session, modelRef string, events EventSink) (*tools.ToolCallResult, error) {
	if sess == nil {
		return tools.ResultError("model_picker needs a session to change the model of"), nil
	}
	handle, err := r.SessionByID(sess.ID)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("failed to set model: %v", err)), nil
	}
	local, ok := handle.(*sessionHandle)
	if !ok {
		return tools.ResultError("failed to set model: session is not a local session"), nil
	}
	if err := local.SetModel(ctx, modelRef); err != nil {
		return tools.ResultError(fmt.Sprintf("failed to set model: %v", err)), nil
	}
	local.EmitPinnedAgentInfo(ctx, events)

	if modelRef == "" {
		slog.InfoContext(ctx, "Model reverted via model_picker tool", "agent", local.AgentName(), "session_id", sess.ID)
		return tools.ResultSuccess("Model reverted to the agent's default model"), nil
	}
	slog.InfoContext(ctx, "Model changed via model_picker tool", "agent", handle.AgentName(), "session_id", sess.ID, "model", modelRef)
	return tools.ResultSuccess("Model changed to " + modelRef), nil
}
