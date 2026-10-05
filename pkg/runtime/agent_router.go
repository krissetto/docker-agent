package runtime

import (
	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// agentRouter contains configuration defaults only. Running sessions resolve
// their owner-pinned agent and never mutate a runtime-wide current agent.
type agentRouter struct {
	team    *team.Team
	initial string
}

func newAgentRouter(t *team.Team, initial string) *agentRouter {
	return &agentRouter{team: t, initial: initial}
}
func (r *agentRouter) Name() string { return r.initial }

// Current returns the current agent. The returned agent is non-nil
// because NewLocalRuntime validates the initial name and Set callers
// either use SetValidated or have already validated against the team.
func (r *agentRouter) Current() *agent.Agent {
	a, _ := r.team.Agent(r.Name())
	return a
}

// ResolveSession returns the explicitly pinned agent for sessions. Legacy
// non-execution helpers may pass an unbound session and use the configured
// default/current metadata; driver admission pins AgentName before execution.
func (r *agentRouter) ResolveSession(sess *session.Session) *agent.Agent {
	if sess != nil && sess.AgentName != "" {
		if a, err := r.team.Agent(sess.AgentName); err == nil {
			return a
		}
	}
	return r.Current()
}
