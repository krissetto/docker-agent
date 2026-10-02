package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
)

// legacyRunHandle adopts only an unbound stored template. Once bound, all
// endpoints share the immutable canonical identity, including old URLs naming
// another source/agent (which upstream ignored for an already-active runtime).
func (sm *SessionManager) legacyRunHandle(ctx context.Context, id, sourceName, agentName string) (runtime.SessionHandle, error) {
	if h := sm.loadedSession(id); h != nil {
		return h, nil
	}
	unlock := sm.sessionRestoreLocks.lock(id)
	defer unlock()
	if h := sm.loadedSession(id); h != nil {
		return h, nil
	}
	sess, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.ParentID != "" || sess.AttributesSnapshot()[sessionAgentAttribute] != "" {
		// Handle takes this same per-root fence. Return to the wrapper so it
		// resolves after our deferred unlock, never recursively under this lock.
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorConflict, SessionID: id, Operation: "legacy_bound_lookup"}
	}
	source, err := sm.resolveSource(sourceName)
	if err != nil {
		return nil, err
	}
	resolvedName := sourceName
	if _, ok := sm.Sources[resolvedName]; !ok {
		for key := range sm.Sources {
			if config.StableSourceKey(key) == config.StableSourceKey(sourceName) {
				resolvedName = key
				break
			}
		}
	}
	registry, canonicalSource, err := sm.sessionRegistryForCreate(resolvedName)
	if err != nil && len(sm.sessionRegistries) == 0 {
		registry, canonicalSource, err = sm.sessionRegistryForCreate("")
	}
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(ctx, source)
	if err != nil {
		return nil, sm.sourceLoadError(sourceName, err)
	}
	if agentName == "" {
		if len(cfg.Agents) == 0 {
			return nil, errors.New("no agents loaded")
		}
		agentName = cfg.Agents[0].Name
		for _, agent := range cfg.Agents {
			if agent.Name == "root" {
				agentName = "root"
				break
			}
		}
	}
	found := false
	for _, agent := range cfg.Agents {
		if agent.Name == agentName {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("agent not found: %s", agentName)
	}
	// Never mutate the store's pointer. CreateSession publishes binding and
	// transcript using the canonical runtime's transactional adoption path.
	sess = sess.Clone()
	sess.AgentName = agentName
	sess.SetAttribute(sessionAgentAttribute, agentName)
	if canonicalSource != "" {
		sess.SetAttribute(sessionSourceAttribute, canonicalSource)
	}
	if sess.GetSafetyPolicy() == "" && !sess.ToolsApproved {
		if defaults, ok := registry.(runtime.SafetyDefaults); ok {
			if policy := authorSafetyDefault(ctx, defaults, sess); policy != "" {
				sess.SetSafetyPolicy(policy)
			}
		}
	}
	return sm.createHTTPSession(ctx, registry, sess, runtime.SessionBinding{AgentName: agentName, Model: sess.AgentModelOverrides[agentName]})
}

func (sm *SessionManager) resolveLegacyRun(ctx context.Context, id, source, agent string) (runtime.SessionHandle, error) {
	if h := sm.loadedSession(id); h != nil {
		return h, nil
	}
	sess, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.ParentID != "" || sess.AttributesSnapshot()[sessionAgentAttribute] != "" {
		return sm.Handle(ctx, id)
	}
	h, err := sm.legacyRunHandle(ctx, id, source, agent)
	if err != nil {
		var typed *runtime.SessionError
		if errors.As(err, &typed) && typed.Operation == "legacy_bound_lookup" {
			return sm.Handle(ctx, id)
		}
	}
	return h, err
}

// rejectUnsupportedLegacyCommands protects custom handles lacking command
// admission. Builtin handles resolve and execute commands in their canonical
// driver; this read-only fallback only diagnoses a missing custom capability.
func (sm *SessionManager) rejectUnsupportedLegacyCommands(ctx context.Context, h runtime.SessionHandle, sourceName string, messages []api.Message) error {
	var commandNames []string
	for _, message := range messages {
		if message.Role != chat.MessageRoleUser || !strings.HasPrefix(message.Content, "/") {
			continue
		}
		head, _, _ := strings.Cut(message.Content, " ")
		commandNames = append(commandNames, strings.TrimPrefix(head, "/"))
	}
	if len(commandNames) == 0 {
		return nil
	}
	if snapshot, err := h.Snapshot(ctx); err == nil {
		if source := snapshot.AttributesSnapshot()[sessionSourceAttribute]; source != "" {
			sourceName = source
		}
	}
	cfg, err := sm.LoadAgentConfig(ctx, sourceName)
	if err != nil {
		return err
	}
	for _, agent := range cfg.Agents {
		if agent.Name != h.AgentName() {
			continue
		}
		for _, name := range commandNames {
			command, ok := agent.Commands[name]
			if !ok {
				for _, group := range agent.UseCommands {
					if candidate, exists := cfg.Commands[group][name]; exists {
						command, ok = candidate, true
					}
				}
			}
			if ok && command.Agent != "" {
				return &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: h.ID(), Operation: "legacy_agent_switch", Detail: "this custom session handle cannot admit legacy agent commands"}
			}
		}
	}
	return nil
}
