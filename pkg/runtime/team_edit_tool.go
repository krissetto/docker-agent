package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/teamedit"
)

func (r *LocalRuntime) teamEditToolset(sourceID string) tools.ToolSet {
	if r.teamEdit == nil || r.teamEdit.editor == nil || r.teamEnvironments == nil {
		return nil
	}
	return &teamedit.ToolSet{Handler: tools.NewRuntimeHandler(func(ctx context.Context, call teamedit.Call, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleTeamEdit(ctx, sourceID, call)
	})}
}

func (r *LocalRuntime) handleTeamEdit(ctx context.Context, sourceID string, call teamedit.Call) (*tools.ToolCallResult, error) {
	if r.teamEdit == nil || r.teamEdit.editor == nil || r.teamEnvironments == nil {
		return nil, fmt.Errorf("team editing is unavailable")
	}
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok || identity.driver == nil || identity.environment == nil {
		return nil, fmt.Errorf("team edit requires an active session-bound environment")
	}
	if identity.environment.Environment().SourceID() != sourceID {
		return nil, fmt.Errorf("session source does not match the editor source")
	}
	var sess *session.Session
	if err := identity.driver.ownerCall(ctx, func() error {
		if identity.driver.generation != identity.generation || identity.driver.sess == nil {
			return ErrSessionStopped
		}
		sess = identity.driver.sess.Clone()
		return nil
	}); err != nil {
		return nil, err
	}
	if recorded, _ := sess.Attribute("docker-agent.actor.source"); recorded != "" && recorded != sourceID {
		return nil, fmt.Errorf("session source identity does not match the active team editor")
	}
	actor, err := teamEditActorFromSession(identity.environment.Environment(), sess)
	if err != nil {
		return nil, err
	}
	if r.teamEdit.grant.Mode == "off" {
		return nil, fmt.Errorf("team editing is disabled")
	}
	if r.teamEdit.grant.AllowedAgentIDsSet && !containsString(r.teamEdit.grant.AllowedAgentIDs, actor.AgentKey) {
		return nil, fmt.Errorf("session agent is outside the trusted team-edit allowlist")
	}
	currentLease, err := r.teamEnvironments.AcquireCurrent()
	if err != nil {
		return nil, err
	}
	currentEnv := currentLease.Environment()
	defer currentLease.Release(context.WithoutCancel(ctx))
	if currentEnv.SourceID() != sourceID || currentEnv.BaselineID() != identity.environment.Environment().BaselineID() {
		return nil, fmt.Errorf("team source revision is no longer part of this session's baseline")
	}
	base := teamdef.Revision{ID: currentEnv.RevisionID(), Generation: revisionGeneration(currentEnv.RevisionID())}
	if editorHead := r.teamEdit.editor.CurrentRevision(); editorHead.ID != base.ID {
		return nil, fmt.Errorf("team editor and runtime environment heads are inconsistent")
	}
	if call.Revision != "" && call.Revision != base.ID {
		return nil, fmt.Errorf("team source revision conflict; inspect the current revision and retry")
	}
	if call.Action == "persist" {
		if call.Revision == "" || call.Revision != base.ID {
			return nil, fmt.Errorf("team source revision conflict; inspect the current revision and retry")
		}
		receipt, err := persistTeamEditSource(ctx, r.teamEdit.editor, base, r.teamEdit.persistence)
		if err != nil {
			return nil, err
		}
		return tools.ResultJSON(receipt), nil
	}
	if call.Action == "export" {
		if call.Revision == "" || call.Revision != base.ID {
			return nil, fmt.Errorf("team source revision conflict; inspect the current revision and retry")
		}
		if r.teamEdit.exportRoot == "" {
			return nil, fmt.Errorf("team export is unavailable")
		}
		path := call.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.teamEdit.exportRoot, path)
		}
		receipt, err := exportTeamEditSource(r.teamEdit.editor, base, path)
		if err != nil {
			return nil, err
		}
		return tools.ResultJSON(receipt), nil
	}
	if call.Action == "inspect" {
		inspection, err := r.teamEdit.editor.Inspect(ctx, base, actor)
		if err != nil {
			return nil, err
		}
		return tools.ResultJSON(inspection), nil
	}
	if call.Action == "apply" {
		patch := teamdef.AgentPatch{AgentKey: call.AgentKey, RenameTo: call.RenameTo, Instruction: call.Instruction, Description: call.Description, WelcomeMessage: call.WelcomeMessage, Model: call.Model, FallbackModels: call.FallbackModels, ModelPickerModels: call.ModelPickerModels, CloneFrom: call.CloneFrom, NewAgentKey: call.NewAgentKey, Remove: call.Remove}
		result, err := r.applyTeamEdit(ctx, identity, base, actor, patch)
		if err != nil {
			return nil, err
		}
		return tools.ResultJSON(result), nil
	}
	if call.Action == "replace" {
		result, err := r.replaceTeamEdit(ctx, identity, base, actor, []byte(call.YAML), call.DryRun)
		if err != nil {
			return nil, err
		}
		return tools.ResultJSON(result), nil
	}
	return nil, fmt.Errorf("unknown team-edit action")
}

func (r *LocalRuntime) buildAndPublishEditedEnvironment(ctx context.Context, identity executionIdentity, result teamdef.EditResult, candidate config.Source, commitSource func() error) error {
	old := identity.environment.Environment()
	recorder, ok := r.sessionStore.(session.TeamEditRecorder)
	if !ok {
		return errors.New("team edit requires durable session history")
	}
	previous, err := r.teamEdit.editor.Source().SourceForLoad(result.BaseRevision)
	if err != nil {
		return err
	}
	fields, err := changedTeamFields(ctx, previous, candidate)
	if err != nil {
		return err
	}
	sessionID := identity.driver.sessionID()
	loaderOptions := append([]teamloader.Opt(nil), r.teamEdit.loaderOptions...)
	if r.teamEdit.toolsetPool != nil {
		loaderOptions = append(loaderOptions, teamloader.WithToolsetLeasePool(r.teamEdit.toolsetPool, r.teamEdit.poolScope))
	}
	next, err := r.teamEdit.builder(ctx, candidate, result.Revision.ID, old.SourceID(), old.BaselineID(), r.teamEdit.runConfig, loaderOptions)
	if err != nil {
		return err
	}
	if next == nil {
		return fmt.Errorf("team environment builder returned nil")
	}
	if next.TeamID() != result.TeamID || next.SourceID() != old.SourceID() || next.BaselineID() != old.BaselineID() || next.RevisionID() != result.Revision.ID {
		_ = next.Discard(context.WithoutCancel(ctx))
		return fmt.Errorf("prepared team environment identity does not match the edit receipt")
	}
	publish := func() error {
		err := recorder.RecordTeamEdit(ctx, session.TeamEditRecord{Revision: result.Revision.ID, ParentRevision: result.BaseRevision.ID, TeamID: result.TeamID, SessionID: sessionID, Actor: result.ActorStableID, Operation: string(result.Operation), ChangedFields: fields})
		if err != nil {
			return fmt.Errorf("recording team edit: %w", err)
		}
		return commitSource()
	}
	if err := r.teamEnvironments.PublishWithCommit(old.SourceID(), old.RevisionID(), next, publish); err != nil {
		_ = next.Discard(context.WithoutCancel(ctx))
		return err
	}
	_ = identity.driver.ownerCall(context.WithoutCancel(ctx), func() error {
		if identity.driver.generation == identity.generation {
			identity.driver.teamEditStopPending = true
		}
		return nil
	})
	return nil
}

func (r *LocalRuntime) applyTeamEdit(ctx context.Context, identity executionIdentity, base teamdef.Revision, actor teamdef.ActorRef, patch teamdef.AgentPatch) (teamdef.EditResult, error) {
	var result teamdef.EditResult
	var err error
	publish := func(receipt teamdef.EditResult, source config.Source, commit func() error) error {
		result = receipt
		return r.buildAndPublishEditedEnvironment(ctx, identity, receipt, source, commit)
	}
	if r.teamEdit.grant.Mode == "restricted" {
		result, err = r.teamEdit.editor.ApplyCoordinated(ctx, base, actor, patch, publish)
	} else {
		return result, fmt.Errorf("full authority requires whole-document replacement")
	}
	return result, err
}

func (r *LocalRuntime) replaceTeamEdit(ctx context.Context, identity executionIdentity, base teamdef.Revision, actor teamdef.ActorRef, data []byte, dryRun bool) (teamdef.EditResult, error) {
	publish := func(receipt teamdef.EditResult, source config.Source, commit func() error) error {
		return r.buildAndPublishEditedEnvironment(ctx, identity, receipt, source, commit)
	}
	return r.teamEdit.editor.ReplaceCoordinated(ctx, base, actor, data, dryRun, publish)
}

func teamEditActorFromSession(env *TeamEnvironment, sess *session.Session) (teamdef.ActorRef, error) {
	if env == nil || sess == nil {
		return teamdef.ActorRef{}, fmt.Errorf("team edit actor identity is unavailable")
	}
	id := sess.AgentID
	if id == "" {
		id, _ = sess.Attribute(SessionAgentIDAttribute)
	}
	agentName := sess.AgentName
	if agentName == "" {
		agentName, _ = sess.Attribute(SessionAgentAttribute)
	}
	if id != "" {
		if _, _, ok := env.Team().ResolveIdentity(id); !ok {
			return teamdef.ActorRef{}, fmt.Errorf("session agent identity is not in the current team")
		}
	} else {
		if _, _, ok := env.Team().ResolveLegacyName(agentName); !ok {
			return teamdef.ActorRef{}, fmt.Errorf("legacy session agent name is not in the current team")
		}
		id = agentName
	}
	if agentName == "" {
		return teamdef.ActorRef{}, fmt.Errorf("session agent identity is not in the current team")
	}
	return teamdef.ActorRef{TeamID: env.TeamID(), AgentKey: id, SourceRevision: env.BaselineID()}, nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (r *LocalRuntime) teamEditDefinitions(ctx context.Context, sess *session.Session, a *agent.Agent) []tools.Tool {
	if r.teamEdit == nil || r.teamEnvironments == nil || a == nil || r.teamEdit.grant.Mode == "off" {
		return nil
	}
	env := teamEnvironmentFromContext(ctx)
	if env == nil {
		env = r.envFromContext(ctx)
	}
	if env == nil {
		return nil
	}
	cfg, ok := env.Team().AgentConfig(a.Name())
	if !ok {
		return nil
	}
	requested := false
	for _, set := range cfg.Toolsets {
		if set.Type == teamedit.ToolsetType {
			requested = true
			break
		}
	}
	if !requested {
		return nil
	}
	if sess == nil {
		return nil
	}
	actorID := sess.AgentID
	if actorID == "" {
		actorID, _ = sess.Attribute(SessionAgentIDAttribute)
	}
	actorName := sess.AgentName
	if actorName == "" {
		actorName, _ = sess.Attribute(SessionAgentAttribute)
	}
	if actorID == "" {
		actorID = actorName
	}
	var actor *agent.Agent
	var found bool
	if sess.AgentID != "" {
		actor, _, found = env.Team().ResolveIdentity(sess.AgentID)
	} else {
		actor, _, found = env.Team().ResolveLegacyName(actorName)
	}
	if !found || actor == nil {
		return nil
	}
	if r.teamEdit.grant.AllowedAgentIDsSet && !containsString(r.teamEdit.grant.AllowedAgentIDs, actorID) {
		return nil
	}
	return teamedit.Definitions(tools.NewRuntimeHandler(func(ctx context.Context, call teamedit.Call, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleTeamEdit(ctx, env.SourceID(), call)
	}))
}

func revisionGeneration(id string) uint64 {
	var generation uint64
	_, _ = fmt.Sscanf(id, "%d:", &generation)
	return generation
}

// Compare authored fields, stopping before values (especially env, tools and credentials).
func changedTeamFields(ctx context.Context, before, after config.Source) ([]string, error) {
	fields := func(src config.Source) (map[string]any, error) {
		raw, err := src.Read(ctx)
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, errors.New("cannot inspect edited team fields")
		}
		agents, _ := doc["agents"].(map[string]any)
		delete(doc, "agents")
		for name, entry := range agents {
			key := "agents." + name
			doc[key] = true
			if agent, ok := entry.(map[string]any); ok {
				for k, v := range agent {
					doc[key+"."+k] = v
				}
			} else {
				doc[key] = entry
			}
		}
		return doc, nil
	}
	old, err := fields(before)
	if err != nil {
		return nil, err
	}
	next, err := fields(after)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for k, v := range old {
		if n, ok := next[k]; !ok || !reflect.DeepEqual(v, n) {
			changed[k] = true
		}
	}
	for k := range next {
		if _, ok := old[k]; !ok {
			changed[k] = true
		}
	}
	return slices.Sorted(maps.Keys(changed)), nil
}
