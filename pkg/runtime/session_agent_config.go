package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/docker-agent/pkg/api"
)

// SessionAgentConfigReader resolves named agents only in the session's owning
// team. It does not start toolsets or grant access to the underlying config.
type SessionAgentConfigReader interface {
	SessionAgentConfig(ctx context.Context, name string) (SessionAgentConfig, error)
}

type SessionAgentConfig = api.SessionAgentConfig[AgentConfigInfo]

func (h *sessionHandle) SessionAgentConfig(ctx context.Context, name string) (SessionAgentConfig, error) {
	if err := ctx.Err(); err != nil {
		return SessionAgentConfig{}, err
	}
	if _, err := h.runtime.team.Agent(name); err != nil {
		return SessionAgentConfig{}, &SessionError{Kind: SessionErrorNotFound, SessionID: h.ID(), Operation: "agent_config"}
	}
	snapshot, err := h.Snapshot(ctx)
	if err != nil {
		return SessionAgentConfig{}, err
	}
	info := h.runtime.AgentConfigInfo(ctx, name)
	info.IsCurrent = name == h.AgentName()
	// Unnamed MCP descriptors contain private server hosts or executable names.
	cfg, hasCfg := h.runtime.team.AgentConfig(name)
	for i := range info.Toolsets {
		ts := &info.Toolsets[i]
		if strings.Contains(strings.ToLower(ts.Kind), "mcp") {
			originalName := ts.Name
			ts.Name = "mcp"
			if hasCfg {
				for _, declared := range cfg.Toolsets {
					if declared.Type == "mcp" && declared.Name != "" && declared.Name == originalName {
						ts.Name = declared.Name
						break
					}
				}
			}
		}
	}
	return SessionAgentConfig{SessionID: h.ID(), Source: snapshot.AttributesSnapshot()["docker-agent.actor.source"], AgentName: name, Info: info}, ctx.Err()
}

func (s *remoteSession) SessionAgentConfig(ctx context.Context, name string) (SessionAgentConfig, error) {
	var out SessionAgentConfig
	endpoint := s.endpoint("agent-config") + "?agent=" + url.QueryEscape(name)
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return SessionAgentConfig{}, err
	}
	if out.SessionID != s.ID() || out.AgentName != name || (s.runtime.source != "" && out.Source != s.runtime.source) {
		return SessionAgentConfig{}, protocolError(fmt.Errorf("agent config identity differs from requested session %q", s.ID()))
	}
	return out, nil
}

var (
	_ SessionAgentConfigReader = (*sessionHandle)(nil)
	_ SessionAgentConfigReader = (*remoteSession)(nil)
)
