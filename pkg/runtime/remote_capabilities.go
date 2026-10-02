package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func (s *remoteSession) InspectTools(ctx context.Context) (SessionToolsInfo, error) {
	var out SessionToolsInfo
	if !s.Metadata().Capabilities.ToolInspection {
		return out, sessionUnsupported(s.ID(), "inspect_tools")
	}
	err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("tools"), nil, &out)
	return out, err
}
func (s *remoteSession) RestartToolset(ctx context.Context, name string) error {
	if !s.Metadata().Capabilities.ToolsetRestart {
		return sessionUnsupported(s.ID(), "restart_toolset")
	}
	return s.runtime.client.sessionWaitJSON(ctx, http.MethodPost, s.endpoint("toolsets/restart"), struct {
		Name string `json:"name"`
	}{name}, nil)
}
func (s *remoteSession) EffectivePermissions(ctx context.Context) (SessionPermissionsInfo, error) {
	var out SessionPermissionsInfo
	if !s.Metadata().Capabilities.PermissionsInspection {
		return out, sessionUnsupported(s.ID(), "permissions")
	}
	err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("permissions"), nil, &out)
	return out, err
}
func (s *remoteSession) MCPPrompts(ctx context.Context) (map[string]tools.PromptInfo, error) {
	if !s.Metadata().Capabilities.MCPPrompts {
		return nil, sessionUnsupported(s.ID(), "mcp_prompts")
	}
	var out map[string]tools.PromptInfo
	err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("mcp/prompts"), nil, &out)
	return out, err
}
func (s *remoteSession) ExecuteMCPPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	if !s.Metadata().Capabilities.MCPPrompts {
		return "", sessionUnsupported(s.ID(), "mcp_prompt")
	}
	var out api.SessionPromptResult
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("mcp/prompts/execute"), api.SessionPromptRequest{Name: name, Args: args}, &out)
	return out.Text, err
}
func (s *remoteSession) editTodo(ctx context.Context, id string, req api.SessionTodoPatch) ([]session.Todo, error) {
	if !s.Metadata().Capabilities.TodoEditing {
		return nil, sessionUnsupported(s.ID(), "edit_todo")
	}
	var out []session.Todo
	err := s.runtime.client.sessionJSON(ctx, http.MethodPatch, s.endpoint("todos/"+url.PathEscape(id)), req, &out)
	return out, err
}
func (r *SessionTransport) BranchSession(ctx context.Context, id string, options BranchOptions) (SessionHandle, *session.Session, error) {
	handle, err := r.SessionByID(id)
	if err != nil {
		return nil, nil, err
	}
	remote := handle.(*remoteSession)
	if err := remote.Hydrate(ctx); err != nil {
		return nil, nil, err
	}
	if !remote.Metadata().Capabilities.Branching {
		return nil, nil, sessionUnsupported(id, "branch")
	}
	var out api.SessionBranchResult
	if err := r.client.sessionJSON(ctx, http.MethodPost, remote.endpoint("branches"), options, &out); err != nil {
		return nil, nil, err
	}
	if out.Session == nil || out.Metadata.SessionID == "" || out.Session.ID != out.Metadata.SessionID {
		return nil, nil, errors.New("invalid branch response identity")
	}
	return &remoteSession{runtime: r, sessionID: out.Metadata.SessionID, metadata: remoteSessionMetadata(out.Metadata).runtime()}, out.Session, nil
}

func (s *remoteSession) SessionAgentInfo(ctx context.Context) (SessionAgentInfo, error) {
	var out SessionAgentInfo
	err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("agent-info"), nil, &out)
	return out, err
}
func (s *remoteSession) EmitPinnedAgentInfo(ctx context.Context, sink EventSink) {
	if sink == nil {
		return
	}
	info, err := s.SessionAgentInfo(ctx)
	if err != nil {
		// Old peers can still display their bound identity without global config.
		metadata := s.Metadata()
		if ctx.Err() == nil {
			sink.Emit(AgentInfo(metadata.AgentName, metadata.Model, "", ""))
		}
		return
	}
	if info.Agent != nil {
		sink.Emit(info.Agent)
	}
	if info.Team != nil {
		sink.Emit(info.Team)
	}
}

func (r *SessionTransport) CreateSessionWithID(ctx context.Context, sess *session.Session, binding SessionBinding, id string) (SessionHandle, error) {
	if id == "" || sess == nil || binding.ParentSessionID != "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "create_session_with_id"}
	}
	info, err := r.client.ServerInfo(ctx)
	if err != nil {
		var response *sessionHTTPError
		if errors.As(err, &response) && (response.status == http.StatusNotFound || response.status == http.StatusNotImplemented) {
			return nil, sessionUnsupported(id, "create_session_with_id")
		}
		return nil, err
	}
	if !slices.Contains(info.Capabilities, "session_explicit_ids") {
		return nil, sessionUnsupported(id, "create_session_with_id")
	}
	return r.createSession(ctx, sess, binding, id)
}
