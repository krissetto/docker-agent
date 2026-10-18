package runtime

import (
	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// configuredAgentID returns only an ID retained from the canonical current
// team configuration. Runtime callers cannot supply or override this value.
func configuredAgentID(tm *team.Team, name string) string {
	if tm == nil {
		return ""
	}
	cfg, ok := tm.AgentConfig(name)
	if !ok {
		return ""
	}
	return cfg.ID
}

func resolveAgentIdentity(tm *team.Team, id, legacyName string) (*agent.Agent, string, bool) {
	if tm == nil {
		return nil, "", false
	}
	if id != "" {
		return tm.ResolveIdentity(id)
	}
	return tm.ResolveLegacyName(legacyName)
}

// stampCreationIdentity records immutable provenance from the canonical team,
// replacing any identity metadata supplied on the caller's session template.
func (r *LocalRuntime) stampCreationIdentity(sess *session.Session, name string) {
	if sess == nil {
		return
	}
	sess.SetAttribute(SessionAgentAttribute, name)
	sess.SetAttribute(SessionAgentIDAttribute, configuredAgentID(r.team, name))
}

// bindExecutionIdentity maps an active session identity to the current authored
// key. Stable IDs are authoritative; only ID-less legacy records use exact names.
func bindExecutionIdentity(tm *team.Team, sess *session.Session) (string, bool) {
	if sess == nil {
		return "", false
	}
	a, name, ok := resolveAgentIdentity(tm, sess.AgentID, sess.AgentName)
	if !ok || a == nil {
		return "", false
	}
	sess.AgentName = name
	return name, true
}

func identityForBinding(tm *team.Team, sess *session.Session, nameAttr, idAttr string) (*agent.Agent, string, bool) {
	if sess == nil {
		return nil, "", false
	}
	id, _ := sess.Attribute(idAttr)
	name, _ := sess.Attribute(nameAttr)
	return resolveAgentIdentity(tm, id, name)
}

func (r *LocalRuntime) allowedIdentity(grant *agent.Agent, child *agent.Agent) bool {
	return r.allowedIdentityForTeam(r.team, grant, child)
}

func (r *LocalRuntime) allowedIdentityForTeam(tm *team.Team, grant *agent.Agent, child *agent.Agent) bool {
	if grant == nil || child == nil || tm == nil {
		return false
	}
	childID := configuredAgentID(tm, child.Name())
	for _, ref := range grant.AsyncSubagents() {
		target, err := tm.Agent(ref.Agent)
		if err != nil {
			continue
		}
		targetID := configuredAgentID(tm, target.Name())
		if childID != "" {
			if targetID == childID {
				return true
			}
		} else if target.Name() == child.Name() {
			return true
		}
	}
	return false
}

func (r *LocalRuntime) currentCreationAgent(sess *session.Session) (*agent.Agent, string, bool) {
	if a, name, ok := identityForBinding(r.team, sess, SessionAgentAttribute, SessionAgentIDAttribute); ok {
		return a, name, true
	}
	if sess == nil {
		return nil, "", false
	}
	id, _ := sess.Attribute(SessionAgentIDAttribute)
	if id != "" {
		return nil, "", false
	}
	name, _ := sess.Attribute(SessionAgentAttribute)
	if name == "" {
		name = sess.AgentName
	}
	return r.teamForContext(nil, sess).ResolveLegacyName(name)
}

func (r *LocalRuntime) currentActiveAgent(sess *session.Session) (*agent.Agent, string, bool) {
	if sess == nil {
		return nil, "", false
	}
	tm := r.teamForSession(sess)
	if tm == nil {
		return nil, "", false
	}
	if sess.AgentID != "" {
		return tm.ResolveIdentity(sess.AgentID)
	}
	name := sess.AgentName
	if name == "" {
		name, _ = sess.Attribute(SessionAgentAttribute)
	}
	return tm.ResolveLegacyName(name)
}

func (r *LocalRuntime) currentGrantorAgent(sess *session.Session, fallback *session.Session) (*agent.Agent, string, bool) {
	if sess != nil {
		id, _ := sess.Attribute(SessionParentAgentIDAttribute)
		name, _ := sess.Attribute(SessionParentAgentAttribute)
		if id != "" || name != "" {
			return resolveAgentIdentity(r.teamForSession(sess), id, name)
		}
	}
	return r.currentActiveAgent(fallback)
}
