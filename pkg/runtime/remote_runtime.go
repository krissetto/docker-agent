package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// RemoteServices provides source-wide presentation metadata. It deliberately
// has no session identity: remote session work is exclusively SessionHandle.
type RemoteServices struct {
	client        RemoteClient
	currentAgent  string
	agentFilename string
	team          *team.Team
}

type RemoteServicesOption func(*RemoteServices)

func WithRemoteCurrentAgent(name string) RemoteServicesOption {
	return func(r *RemoteServices) { r.currentAgent = name }
}

func WithRemoteAgentFilename(filename string) RemoteServicesOption {
	return func(r *RemoteServices) { r.agentFilename = filename }
}

func NewRemoteServices(client RemoteClient, opts ...RemoteServicesOption) (*RemoteServices, error) {
	if client == nil {
		return nil, errors.New("client cannot be nil")
	}
	r := &RemoteServices{client: client, agentFilename: "agent.yaml", team: team.New()}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

func (r *RemoteServices) config(ctx context.Context) *latest.Config {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get remote agent configuration", "error", err)
		return nil
	}
	return cfg
}

func (r *RemoteServices) resolvedAgent(ctx context.Context) (string, latest.AgentConfig) {
	cfg := r.config(ctx)
	if cfg == nil || len(cfg.Agents) == 0 {
		return r.currentAgent, latest.AgentConfig{}
	}
	name := cmp.Or(r.currentAgent, cfg.Agents[0].Name)
	for _, agent := range cfg.Agents {
		if agent.Name == name {
			return name, agent
		}
	}
	return name, latest.AgentConfig{}
}

func (r *RemoteServices) CurrentAgentName(ctx context.Context) string {
	name, _ := r.resolvedAgent(ctx)
	return name
}

func (r *RemoteServices) CurrentAgentInfo(ctx context.Context) CurrentAgentInfo {
	name, cfg := r.resolvedAgent(ctx)
	return CurrentAgentInfo{Name: name, Description: cfg.Description, Commands: cfg.Commands}
}

func (r *RemoteServices) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, ErrUnsupported
}
func (r *RemoteServices) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }
func (r *RemoteServices) RestartToolset(context.Context, string) error       { return ErrUnsupported }

func (r *RemoteServices) EmitStartupInfo(ctx context.Context, _ *session.Session, sink EventSink) {
	r.EmitAgentInfo(ctx, sink)
}

func (r *RemoteServices) EmitAgentInfo(ctx context.Context, sink EventSink) {
	name, cfg := r.resolvedAgent(ctx)
	sink.Emit(AgentInfo(name, cfg.Model, cfg.Description, cfg.WelcomeMessage))
	sink.Emit(TeamInfo(r.agentDetails(ctx), name))
}

func (r *RemoteServices) agentDetails(ctx context.Context) []AgentDetails {
	cfg := r.config(ctx)
	if cfg == nil {
		return nil
	}
	out := make([]AgentDetails, 0, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		detail := AgentDetails{Name: agent.Name, Description: agent.Description, Commands: agent.Commands}
		if provider, model, ok := strings.Cut(agent.Model, "/"); ok {
			detail.Provider, detail.Model = provider, model
		} else {
			detail.Model = agent.Model
		}
		out = append(out, detail)
	}
	return out
}

func (r *RemoteServices) ResetStartupInfo()                          {}
func (r *RemoteServices) SessionStore() session.Store                { return nil }
func (r *RemoteServices) PermissionsInfo() *PermissionsInfo          { return nil }
func (r *RemoteServices) CurrentAgentSkillsToolset() *skills.ToolSet { return nil }
func (r *RemoteServices) CurrentMCPPrompts(context.Context) map[string]tools.PromptInfo {
	return nil
}

func (r *RemoteServices) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", ErrUnsupported
}

func (r *RemoteServices) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return errors.New("session title changes require SessionHandle")
}
func (r *RemoteServices) OnToolsChanged(func(Event))       {}
func (r *RemoteServices) OnBackgroundEvent(func(Event))    {}
func (r *RemoteServices) OnElicitationRequest(func(Event)) {}
func (r *RemoteServices) Close() error                     { return nil }

func (r *RemoteServices) AgentTools(context.Context, string) ([]tools.Tool, error) {
	return nil, ErrUnsupported
}

func (r *RemoteServices) AgentCommands(ctx context.Context, agentName string) (types.Commands, error) {
	cfg := r.config(ctx)
	if cfg != nil {
		for _, agent := range cfg.Agents {
			if agent.Name == agentName {
				return agent.Commands, nil
			}
		}
	}
	return nil, fmt.Errorf("agent %q not found", agentName)
}

func (r *RemoteServices) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }

func (r *RemoteServices) String() string {
	return fmt.Sprintf("remote source %q", r.agentFilename)
}
