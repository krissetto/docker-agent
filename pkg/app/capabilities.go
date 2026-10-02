package app

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// InspectTools reads one bound-agent snapshot. A bound handle is authoritative:
// old peers never fall through to process-global presentation services.
func (a *App) InspectTools(ctx context.Context) (runtime.SessionToolsInfo, error) {
	if h := a.SessionHandle(); h != nil {
		if p, ok := h.(runtime.SessionToolInspector); ok && h.Metadata().Capabilities.ToolInspection {
			return p.InspectTools(ctx)
		}
		return runtime.SessionToolsInfo{}, runtime.UnsupportedSessionOperation(h.ID(), "inspect_tools")
	}
	available, err := a.runtime.CurrentAgentTools(ctx)
	if err != nil {
		return runtime.SessionToolsInfo{}, err
	}
	info := runtime.SessionToolsInfo{Tools: available}
	for _, s := range a.runtime.CurrentAgentToolsetStatuses() {
		last := ""
		if s.LastError != nil {
			last = "toolset unavailable"
		}
		info.Statuses = append(info.Statuses, runtime.SessionToolsetStatus{Name: s.Name, Kind: s.Kind, Description: s.Description, State: s.State, LastError: last, RestartCount: s.RestartCount, Restartable: s.Restartable})
	}
	return info, nil
}

// ToolsetStatuses converts portable sanitized metadata for existing dialogs.
func ToolsetStatuses(info runtime.SessionToolsInfo) []tools.ToolsetStatus {
	out := make([]tools.ToolsetStatus, 0, len(info.Statuses))
	for _, s := range info.Statuses {
		var err error
		if s.LastError != "" {
			err = errors.New(s.LastError)
		}
		out = append(out, tools.ToolsetStatus{Name: s.Name, Kind: s.Kind, Description: s.Description, State: s.State, LastError: err, RestartCount: s.RestartCount, Restartable: s.Restartable})
	}
	return out
}

func (a *App) EffectivePermissions(ctx context.Context) (runtime.SessionPermissionsInfo, error) {
	if h := a.SessionHandle(); h != nil {
		if p, ok := h.(runtime.SessionPermissionsInspector); ok && h.Metadata().Capabilities.PermissionsInspection {
			return p.EffectivePermissions(ctx)
		}
		return runtime.SessionPermissionsInfo{}, runtime.UnsupportedSessionOperation(h.ID(), "permissions")
	}
	approved, policy, rules := a.Session().SafetySettings()
	return runtime.SessionPermissionsInfo{Policy: policy, ToolsApproved: approved, Source: a.runtime.PermissionsInfo(), Session: rules}, nil
}

func CombinedPermissions(info runtime.SessionPermissionsInfo) *runtime.PermissionsInfo {
	if info.Source == nil && info.Session == nil {
		return nil
	}
	result := &runtime.PermissionsInfo{}
	if p := info.Session; p != nil {
		result.Allow = append(result.Allow, p.Allow...)
		result.Ask = append(result.Ask, p.Ask...)
		result.Deny = append(result.Deny, p.Deny...)
	}
	if p := info.Source; p != nil {
		result.Allow = append(result.Allow, p.Allow...)
		result.Ask = append(result.Ask, p.Ask...)
		result.Deny = append(result.Deny, p.Deny...)
	}
	return result
}

func (a *App) MCPPrompts(ctx context.Context) (map[string]tools.PromptInfo, error) {
	if h := a.SessionHandle(); h != nil {
		if p, ok := h.(runtime.SessionMCPPrompts); ok && h.Metadata().Capabilities.MCPPrompts {
			return p.MCPPrompts(ctx)
		}
		return nil, runtime.UnsupportedSessionOperation(h.ID(), "mcp_prompts")
	}
	return a.runtime.CurrentMCPPrompts(ctx), nil
}

// BranchSession creates only through the shared session owner, never by uploading
// a client transcript or writing a second store. The snapshot proof identifies
// the exact displayed item cut, not a message ordinal.
func (a *App) BranchSession(ctx context.Context, options runtime.BranchOptions) (runtime.SessionHandle, *session.Session, error) {
	h := a.SessionHandle()
	if h == nil {
		return nil, nil, runtime.ErrUnsupported
	}
	if p, ok := a.sessions.(runtime.SessionBrancher); ok && h.Metadata().Capabilities.Branching {
		return p.BranchSession(ctx, h.ID(), options)
	}
	return nil, nil, runtime.UnsupportedSessionOperation(h.ID(), "branch")
}

// CapabilityView captures the current immutable binding for asynchronous
// presentation work. It owns no observation, execution, or runtime lifetime.
// Callers must not Start/Close it or use it to submit conversation input.
func (a *App) CapabilityView() *App {
	return &App{currentState: a.state(), runtime: a.runtime, sessions: a.sessions}
}
