package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

type localSessionRuntimeView struct{ runtime *LocalRuntime }

type localSessionRuntimeSupervisor struct{ runtime *LocalRuntime }

// NewSessionRuntimeSupervisor gives ownership of runtime to an explicit
// supervisor. Existing LocalRuntime construction remains unchanged while
// bootstrap code migrates ownership to this boundary.
func NewSessionRuntimeSupervisor(runtime *LocalRuntime) SessionRuntimeSupervisor {
	return &localSessionRuntimeSupervisor{runtime: runtime}
}

func (s *localSessionRuntimeSupervisor) Runtime() SessionRuntime {
	return &localSessionRuntimeView{runtime: s.runtime}
}

func (s *localSessionRuntimeSupervisor) Shutdown(ctx context.Context) error {
	return s.runtime.shutdownSessions(ctx)
}

func (v *localSessionRuntimeView) CreateSession(ctx context.Context, sess *session.Session, binding SessionBinding) (SessionHandle, error) {
	return v.runtime.CreateSession(ctx, sess, binding)
}

func (v *localSessionRuntimeView) SessionByID(sessionID string) (SessionHandle, error) {
	return v.runtime.SessionByID(sessionID)
}

func (v *localSessionRuntimeView) DeleteSession(ctx context.Context, sessionID string) error {
	return v.runtime.DeleteSession(ctx, sessionID)
}

func (v *localSessionRuntimeView) TitleGenerator(ctx context.Context) *sessiontitle.Generator {
	return v.runtime.TitleGenerator(ctx)
}

func (v *localSessionRuntimeView) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	return v.runtime.UpdateSessionTitle(ctx, sess, title)
}

func (v *localSessionRuntimeView) ListSessions(ctx context.Context) ([]SessionCatalogEntry, error) {
	if v.runtime.sessionStore == nil {
		return nil, ErrUnsupported
	}
	sessions, err := v.runtime.sessionStore.GetSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SessionCatalogEntry, 0, len(sessions))
	for _, sess := range sessions {
		attrs := sess.AttributesSnapshot()
		boundAgent := attrs[SessionAgentAttribute]
		_, loaded := v.runtime.sessionDrivers.Lookup(sess.ID)
		if loaded && boundAgent == "" {
			boundAgent = sess.AgentName
		}
		out = append(out, SessionCatalogEntry{SessionID: sess.ID, Title: sess.TitleSnapshot(), AgentName: boundAgent, UpdatedAt: sess.CreatedAt.Format(time.RFC3339), CreatedAt: sess.CreatedAt, Starred: sess.Starred, Loadable: loaded || boundAgent != "", NumMessages: len(sess.GetAllMessages()), Cost: sess.TotalCost(), WorkingDir: sess.WorkingDir})
	}
	return out, nil
}

func (v *localSessionRuntimeView) ListSessionSummaries(ctx context.Context, options SessionSummaryOptions) ([]SessionSummaryEntry, error) {
	store, ok := v.runtime.sessionStore.(session.ScopedSummaryStore)
	if !ok {
		return nil, UnsupportedSessionOperation("", "session_summaries")
	}
	rows, err := store.GetSessionSummariesWithScope(ctx, session.SummaryScope{IncludeChildren: options.IncludeChildren})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]session.Summary, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			return nil, errors.New("session summary is missing canonical identity")
		}
		if _, duplicate := byID[row.ID]; duplicate {
			return nil, errors.New("duplicate session summary identity")
		}
		byID[row.ID] = row
	}
	// Memoize metadata ancestry once; browsing never loads durable trees or
	// transcripts. Durable child membership remains confirmation-time work.
	routes := make(map[string]string, len(rows))
	var route func(string, map[string]bool) string
	route = func(id string, visiting map[string]bool) string {
		if result, known := routes[id]; known {
			return result
		}
		row, exists := byID[id]
		if !exists {
			return "persisted parent session is missing"
		}
		if visiting[id] {
			return "persisted session ancestry has a cycle"
		}
		visiting[id] = true
		defer delete(visiting, id)
		result := ""
		bound := row.Attributes[SessionAgentAttribute]
		if bound == "" {
			result = "session is not attachable"
		} else if _, err := v.runtime.team.Agent(bound); err != nil {
			result = "persisted session agent is unavailable"
		} else if row.ParentID != "" {
			result = route(row.ParentID, visiting)
			if result == "" && row.Attributes["docker-agent.actor.source"] != "" && row.Attributes["docker-agent.actor.source"] != byID[row.ParentID].Attributes["docker-agent.actor.source"] {
				result = "persisted child session source differs from root source"
			}
		}
		routes[id] = result
		return result
	}
	out := make([]SessionSummaryEntry, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !options.IncludeChildren && row.ParentID != "" {
			continue
		}
		bound := row.Attributes[SessionAgentAttribute]
		entry := SessionSummaryEntry{SessionID: row.ID, ParentID: row.ParentID, Title: row.Title, AgentName: bound, Model: row.AgentModelOverrides[bound], Source: row.Attributes["docker-agent.actor.source"], CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt.Format(time.RFC3339), Starred: row.Starred, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir}
		if driver, loaded := v.runtime.sessionDrivers.Lookup(row.ID); loaded {
			// Reuse immutable owner binding without Snapshot, Observe, or lookup
			// that could initialize providers or adopt stored work.
			driver.mu.Lock()
			if !driver.stopped && !driver.reclaiming {
				entry.Loaded, entry.Loadable = true, true
				entry.AgentName, entry.Model = driver.AgentNameLocked(), driver.modelRef
			}
			driver.mu.Unlock()
		}
		if !entry.Loaded {
			entry.RouteError = route(row.ID, make(map[string]bool))
			if entry.RouteError == "" {
				entry.Loadable = row.ParentID == ""
				entry.RequiresConfirmation = row.ParentID != ""
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func (v *localSessionRuntimeView) LoadSession(ctx context.Context, sessionID string) (SessionHandle, *session.Session, error) {
	v.runtime.creationMu.Lock()
	defer v.runtime.creationMu.Unlock()
	return v.loadSession(ctx, sessionID)
}

func (v *localSessionRuntimeView) loadSession(ctx context.Context, sessionID string) (SessionHandle, *session.Session, error) {
	if found, err := v.SessionByID(sessionID); err == nil {
		if handle, ok := found.(*sessionHandle); ok {
			return found, handle.driver.session().Clone(), nil
		}
	}
	if v.runtime.sessionStore == nil {
		return nil, nil, ErrUnsupported
	}
	sess, err := v.runtime.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	boundAgent := sess.AttributesSnapshot()[SessionAgentAttribute]
	if boundAgent == "" {
		return nil, nil, &SessionError{Kind: SessionErrorUnsupported, SessionID: sessionID, Operation: "attach"}
	}
	if sess.AgentName == "" {
		sess.AgentName = boundAgent
	}
	binding := SessionBinding{AgentName: boundAgent, Model: sess.AgentModelOverrides[boundAgent]}
	handle, err := v.runtime.createSession(ctx, sess, binding)
	return handle, sess, err
}

func (v *localSessionRuntimeView) SwitchAgent(ctx context.Context, sessionID, targetAgent string) (SessionHandle, *session.Session, error) {
	driver, ok := v.runtime.sessionDrivers.Lookup(sessionID)
	if !ok {
		return nil, nil, &SessionError{Kind: SessionErrorNotFound, SessionID: sessionID, Operation: "switch_agent", Reason: SessionErrorReasonBusy}
	}
	original, err := driver.cloneForAgentSwitch(ctx)
	if err != nil {
		return nil, nil, err
	}
	cloned, err := session.BranchSession(original, len(original.MessagesSnapshot()))
	if err != nil {
		return nil, nil, err
	}
	cloned.AgentName = targetAgent
	cloned.SetAttribute(SessionAgentAttribute, targetAgent)
	handle, err := v.CreateSession(ctx, cloned, SessionBinding{AgentName: targetAgent})
	return handle, cloned, err
}

func (v *localSessionRuntimeView) AuthorSafetyDefault(sess *session.Session) session.SafetyPolicy {
	if a, err := v.runtime.team.Agent(sess.AgentName); err == nil && a != nil {
		if s := a.Safety(); s != "" {
			return session.SafetyPolicy(s)
		}
	}
	return session.SafetyPolicy(v.runtime.team.RuntimeSafety())
}

func (v *localSessionRuntimeView) InspectSessionTree(ctx context.Context, rootSessionID string) (*subagent.Snapshot, error) {
	if handle, err := v.runtime.SessionByID(rootSessionID); err == nil {
		local := handle.(*sessionHandle)
		v.runtime.subagents.ensureRoot(local.driver.session(), local.agentName)
	}
	if node, ok := subtreeForSession(v.runtime.subagents.tree.Snapshot(), rootSessionID); ok {
		snapshot := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: node.Node.ID, Nodes: []subagent.NodeSnapshot{node}}
		snapshot.Durability = v.runtime.sessionDurability()
		return &snapshot, nil
	}
	if v.runtime.subagentStore == nil {
		return nil, nil
	}
	return v.runtime.subagentStore.LoadTree(ctx, rootSessionID)
}

func (v *localSessionRuntimeView) RestoreSessionTree(ctx context.Context, root *session.Session) error {
	snapshot, err := v.runtime.RestoreSubagentTree(ctx, root)
	if err != nil {
		return err
	}
	if snapshot != nil {
		root.SetSubagentTree(snapshot)
	}
	return nil
}

// sessionHandle is the stable-ID session-native API. A handle never permits the
// pinned session or agent identity to be replaced.
type sessionHandle struct {
	runtime   *LocalRuntime
	driver    *sessionDriver
	sessionID string
	agentName string

	metadataMu          sync.Mutex
	metadataInitialized bool
	metadataVersion     uint64
	metadataModel       string
	metadataModels      []string
	metadataLevels      []effort.Level
	metadataCurrent     effort.Level
}

// CreateSession returns the stable handle for sess, creating its session if
// needed. Binding identity is immutable and must match the session.
func (r *LocalRuntime) CreateSession(ctx context.Context, sess *session.Session, binding SessionBinding) (SessionHandle, error) {
	r.creationMu.Lock()
	defer r.creationMu.Unlock()
	return r.createSession(ctx, sess, binding)
}

func (r *LocalRuntime) createSession(ctx context.Context, sess *session.Session, binding SessionBinding) (SessionHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sess == nil || sess.ID == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "create_session"}
	}
	if err := r.sessionDrivers.beginClaim(sess.ID); err != nil {
		return nil, err
	}
	defer r.sessionDrivers.releaseUnpublishedClaim(sess.ID)
	if binding.ParentSessionID != "" {
		return r.createClientChild(ctx, sess, binding)
	}
	boundSession, activeAgent, maxIterations := r.sessionDrivers.sessionBindingSnapshot(sess)
	boundAgent := sess.AttributesSnapshot()[SessionAgentAttribute]
	if boundAgent == "" {
		boundAgent = activeAgent
	}
	if binding.Durability == subagent.DurabilityDurable {
		if r.sessionDurability() != subagent.DurabilityDurable {
			return nil, &SessionError{Kind: SessionErrorUnsupported, SessionID: sess.ID, Operation: "durable_session"}
		}
	}
	if binding.AgentName != "" {
		if _, err := r.team.Agent(binding.AgentName); err != nil {
			return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: sess.ID, Operation: "bind_agent"}
		}
	}
	if binding.AgentName != "" && boundAgent != "" && binding.AgentName != boundAgent {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "bind_agent"}
	}
	if boundAgent == "" {
		if binding.AgentName != "" {
			boundAgent = binding.AgentName
		} else {
			defaultAgent, err := r.team.DefaultAgent()
			if err != nil {
				return nil, err
			}
			boundAgent = defaultAgent.Name()
		}
	}
	if maxIterations == 0 {
		if bound, agentErr := r.team.Agent(boundAgent); agentErr == nil {
			maxIterations = bound.MaxIterations()
		}
	}
	if activeAgent == "" {
		activeAgent = boundAgent
	}
	boundSession.AgentName = activeAgent
	boundSession.MaxIterations = maxIterations
	modelRef, providers, err := r.resolveSessionModelBinding(ctx, boundSession, binding.Model)
	if err != nil {
		return nil, err
	}
	boundSession.SetAttribute(SessionAgentAttribute, boundAgent)
	createdRow := false
	if r.sessionStore != nil {
		if stored, err := r.sessionStore.GetSession(ctx, sess.ID); errors.Is(err, session.ErrNotFound) {
			boundSession.SetAttribute(SessionAgentAttribute, boundAgent)
			if err := r.sessionStore.AddSession(ctx, boundSession.OwnSnapshot()); err != nil {
				if !errors.Is(err, session.ErrAlreadyExists) {
					return nil, err
				}
			} else {
				createdRow = true
			}
		} else if err != nil {
			return nil, err
		} else {
			persistedAgent := stored.AttributesSnapshot()[SessionAgentAttribute]
			if persistedAgent != "" && persistedAgent != boundAgent {
				return nil, &SessionError{Kind: SessionErrorConflict, SessionID: sess.ID, Operation: "bind_agent"}
			}
			if persistedAgent == "" {
				if err := r.sessionStore.UpdateSession(ctx, boundSession.OwnSnapshot()); err != nil {
					return nil, err
				}
			}
		}
	}
	if existing, ok := r.sessionDrivers.Lookup(sess.ID); ok && !existing.isStopped() {
		current := existing.session()
		creation := current.AttributesSnapshot()[SessionAgentAttribute]
		if creation == "" {
			creation = current.AgentName
		}
		if creation != boundAgent {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "bind_agent"}
		}
		return &sessionHandle{runtime: r, driver: existing, sessionID: sess.ID, agentName: creation}, nil
	}
	owned := boundSession.Clone()
	d, err := r.sessionDrivers.publishInitializedWithBinding(owned, modelRef, providers, activeAgent, maxIterations, false, true)
	if err != nil {
		if createdRow {
			_ = r.sessionStore.DeleteSession(ctx, sess.ID)
		}
		return nil, err
	}
	return &sessionHandle{runtime: r, driver: d, sessionID: sess.ID, agentName: boundAgent}, nil
}

func (r *LocalRuntime) createClientChild(ctx context.Context, requested *session.Session, binding SessionBinding) (SessionHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.Durability == subagent.DurabilityDurable {
		if r.sessionDurability() != subagent.DurabilityDurable {
			return nil, &SessionError{Kind: SessionErrorUnsupported, SessionID: requested.ID, Operation: "durable_session"}
		}
	}
	parentHandle, err := r.SessionByID(binding.ParentSessionID)
	if err != nil {
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: binding.ParentSessionID, Operation: "create_parent"}
	}
	parent := parentHandle.(*sessionHandle).driver.session()
	parentAgent := r.resolveSessionAgent(parent)
	ref, ok := subagent.FindAllowed(r.allowedFromAgent(parentAgent), binding.AgentName)
	if !ok {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: binding.ParentSessionID, Operation: "subagent_admission"}
	}
	childAgent, err := r.team.Agent(ref.Agent)
	if err != nil {
		return nil, err
	}
	toolsApproved, safetyPolicy, permissions := parent.SafetySettings()
	child := newSubSession(parent, SubSessionConfig{AgentName: ref.Agent, ToolsApproved: toolsApproved, SafetyPolicy: safetyPolicy, Permissions: permissions, NonInteractive: true, PinAgent: true, Model: binding.Model, Title: requested.TitleSnapshot()}, childAgent)
	child.WorkingDir = parent.WorkingDir
	child.SetSafetyPolicy(safetyPolicy)
	child.SetToolsApproved(toolsApproved)
	child.Permissions = session.ClonePermissionsConfig(permissions)
	child.ID, child.AsyncSubagent = requested.ID, true
	requested = child
	if err := r.subagents.registerIdleChild(parent, parentAgent.Name(), requested, childAgent, ref); err != nil {
		return nil, err
	}
	return r.SessionByID(requested.ID)
}

// SessionByID resolves a session previously registered with Session.
func (r *LocalRuntime) SessionByID(sessionID string) (SessionHandle, error) {
	d, ok := r.sessionDrivers.Lookup(sessionID)
	if !ok {
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: sessionID, Operation: "lookup"}
	}
	sess := d.session()
	agentName := ""
	if sess != nil {
		agentName = sess.AgentName
	}
	if d.ModelProvidersEmpty() && r.team != nil {
		if a, err := r.team.Agent(agentName); err == nil {
			d.SetModelBinding("", a.ConfiguredModels())
		}
	}
	return &sessionHandle{runtime: r, driver: d, sessionID: sessionID, agentName: agentName}, nil
}

func (h *sessionHandle) Todos(ctx context.Context) ([]session.Todo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx = httpclient.ContextWithSessionID(ctx, h.sessionID)
	if todoSet := h.todoToolSet(); todoSet != nil {
		return todoSet.Todos(ctx)
	}
	return []session.Todo{}, nil
}

func (h *sessionHandle) todoToolSet() *todotool.ToolSet {
	a := h.runtime.resolveSessionAgent(h.driver.session())
	if a != nil {
		for _, toolset := range a.ToolSets() {
			if todoSet, ok := tools.As[*todotool.ToolSet](toolset); ok {
				if bound := h.runtime.todoToolsets[todoSet]; bound != nil {
					return bound
				}
				return todoSet
			}
		}
	}
	return nil
}

func (h *sessionHandle) SetTodoStatus(ctx context.Context, id, status string) ([]session.Todo, error) {
	switch status {
	case "pending", "in-progress", "completed":
	default:
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: SessionOperationSetTodoStatus, Detail: "invalid todo status"}
	}
	return h.mutateTodo(ctx, id, SessionOperationSetTodoStatus, func(items []session.Todo, index int) ([]session.Todo, error) {
		items[index].Status = status
		return items, nil
	})
}

func (h *sessionHandle) SetTodoDescription(ctx context.Context, id, expectedDescription, description string) ([]session.Todo, error) {
	if strings.TrimSpace(description) == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: SessionOperationSetTodoDescription, Detail: "todo description is required"}
	}
	return h.mutateTodo(ctx, id, SessionOperationSetTodoDescription, func(items []session.Todo, index int) ([]session.Todo, error) {
		if items[index].Description != expectedDescription {
			return nil, &SessionError{Kind: SessionErrorConflict, SessionID: h.sessionID, Operation: SessionOperationSetTodoDescription, Detail: "todo description changed; reload before retrying"}
		}
		items[index].Description = description
		return items, nil
	})
}

func (h *sessionHandle) RemoveTodo(ctx context.Context, id string) ([]session.Todo, error) {
	return h.mutateTodo(ctx, id, SessionOperationRemoveTodo, func(items []session.Todo, index int) ([]session.Todo, error) {
		return slices.Delete(items, index, index+1), nil
	})
}

func (h *sessionHandle) mutateTodo(ctx context.Context, id string, operation SessionOperation, mutate func([]session.Todo, int) ([]session.Todo, error)) ([]session.Todo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: operation, Detail: "todo ID is required"}
	}
	todoSet := h.todoToolSet()
	store, ok := h.runtime.sessionStore.(session.TodoStore)
	if !ok || todoSet == nil {
		return nil, sessionUnsupported(h.sessionID, operation)
	}
	key := h.sessionID
	if todoSet.Shared() {
		key = h.runtime.todoRootSessionID(key)
	}
	items, err := func() ([]session.Todo, error) {
		h.driver.mu.Lock()
		defer h.driver.mu.Unlock()
		if err := h.driver.admitLocked(operation); err != nil {
			return nil, err
		}
		if h.driver.reclaiming {
			return nil, &SessionError{Kind: SessionErrorStopped, SessionID: h.sessionID, Operation: operation}
		}
		return store.MutateTodos(ctx, key, func(items []session.Todo) ([]session.Todo, error) {
			index := slices.IndexFunc(items, func(item session.Todo) bool { return item.ID == id })
			if index < 0 {
				return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: h.sessionID, Operation: operation, Detail: "todo ID not found"}
			}
			return mutate(items, index)
		})
	}()
	if err != nil {
		return nil, err
	}
	h.runtime.publishTodosChanged(key)
	return items, nil
}

func (h *sessionHandle) ID() string        { return h.sessionID }
func (h *sessionHandle) AgentName() string { return h.driver.AgentName() }
func (h *sessionHandle) Metadata() SessionMetadata {
	durability := h.runtime.sessionDurability()
	model, available, levels, current := h.metadataModelBinding()
	modelSwitching := h.runtime.SupportsModelSwitching()
	forkSkills := false
	if st := agentSkillsToolset(h.runtime.resolveSessionAgent(h.driver.session())); st != nil {
		forkSkills = slices.ContainsFunc(st.Skills(), func(skill skills.Skill) bool { return skill.IsFork() })
	}
	return SessionMetadata{SessionID: h.sessionID, AgentName: h.AgentName(), Model: model, ThinkingLevels: levels, ThinkingLevel: current, Capabilities: SessionCapabilities{
		DelegationPolicy: true, StopSubtree: true, ToolInspection: true, ToolsetRestart: true, PermissionsInspection: true, MCPPrompts: true, TodoEditing: true, Branching: true,
		AvailableModels: available, Durability: durability,
		Compaction: true, TargetCompaction: true, ModelSwitching: modelSwitching, ContextInspection: true, LiveSessions: true, SessionEditing: true,
		ForkSkills: forkSkills, Pause: true, ModelCatalogRefresh: modelStoreCanRefresh(h.runtime.modelsStore), ThinkingLevels: modelSwitching && len(levels) > 1, Todos: true,
	}}
}

func (h *sessionHandle) metadataModelBinding() (string, []string, []effort.Level, effort.Level) {
	for {
		version, model, providers := h.driver.ModelBindingSnapshot()
		h.metadataMu.Lock()
		if h.metadataInitialized && h.metadataVersion == version {
			cachedModel := h.metadataModel
			available := slices.Clone(h.metadataModels)
			levels := slices.Clone(h.metadataLevels)
			current := h.metadataCurrent
			h.metadataMu.Unlock()
			return cachedModel, available, levels, current
		}
		h.metadataMu.Unlock()

		var available []string
		if h.runtime.SupportsModelSwitching() {
			available = modelChoiceRefs(h.runtime.availableModels(context.Background(), h.agentName))
		}
		levels, current, err := h.runtime.resolveThinkingLevelsForModels(context.Background(), providers)
		if err != nil {
			levels, current = nil, ""
		}
		latestVersion, _, _ := h.driver.ModelBindingSnapshot()
		if latestVersion != version {
			continue
		}
		h.metadataMu.Lock()
		h.metadataInitialized = true
		h.metadataVersion = version
		h.metadataModel = model
		h.metadataModels = slices.Clone(available)
		h.metadataLevels = slices.Clone(levels)
		h.metadataCurrent = current
		h.metadataMu.Unlock()
		return model, available, levels, current
	}
}

func modelChoiceRefs(choices []ModelChoice) []string {
	refs := make([]string, 0, len(choices))
	for _, choice := range choices {
		refs = append(refs, choice.Ref)
	}
	return refs
}

// Submit accepts input and starts a settled session or appends to its bounded
// mailbox. The returned TurnID is server-assigned and immutable.
func (h *sessionHandle) Submit(ctx context.Context, input TurnInput) (Submission, error) {
	return h.submit(ctx, input, "submit")
}

func (h *sessionHandle) Retry(ctx context.Context) (Submission, error) {
	return h.submit(ctx, TurnInput{Retry: true}, "retry")
}

// Steer records guidance immediately and queues it for the active turn's next
// established safe boundary. It never cancels the turn.
func (h *sessionHandle) Steer(ctx context.Context, input TurnInput) (Submission, error) {
	if err := ctx.Err(); err != nil {
		return Submission{}, err
	}
	turnID, err := sessionInputID(h.sessionID, input.RequestID)
	if err != nil {
		return Submission{}, err
	}
	msg := QueuedMessage{InputOrigin: session.InputOriginUser, Content: input.Content, MultiContent: input.MultiContent, RequestID: turnID, InputMode: "steer"}
	queued, err := h.driver.postSteer(ctx, msg)
	if err != nil {
		return Submission{}, err
	}
	disposition := SubmissionDisposition("")
	if queued {
		disposition = SubmissionDispositionQueued
	}
	return Submission{SessionID: h.sessionID, TurnID: turnID, Disposition: disposition}, nil
}

func (h *sessionHandle) submit(ctx context.Context, input TurnInput, operation string) (Submission, error) {
	msg := QueuedMessage{InputOrigin: session.InputOriginUser, Content: input.Content, MultiContent: input.MultiContent, Retry: input.Retry}
	if err := ctx.Err(); err != nil {
		return Submission{}, err
	}
	requestID, err := sessionInputID(h.sessionID, input.RequestID)
	if err != nil {
		return Submission{}, err
	}
	msg.RequestID = requestID
	if operation == "submit" && !input.Retry {
		if handled, err := h.reactivateStoppedView(ctx, msg); handled {
			if err != nil {
				return Submission{}, err
			}
			h.driver.WakePending()
			return Submission{SessionID: h.sessionID, TurnID: requestID}, nil
		}
	}
	queued, err := h.driver.post(ctx, msg, true)
	if err != nil {
		var sessionErr *SessionError
		if errors.As(err, &sessionErr) && sessionErr.Operation == "post" {
			sessionErrCopy := *sessionErr
			sessionErrCopy.Operation = SessionOperation(operation)
			return Submission{}, &sessionErrCopy
		}
		return Submission{}, err
	}
	disposition := SubmissionDisposition("")
	if queued {
		disposition = SubmissionDispositionQueued
	}
	return Submission{SessionID: h.sessionID, TurnID: requestID, Disposition: disposition}, nil
}

func newSessionRequestID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create session request id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

// Respond resolves a driver-owned pending interaction. RequestID is mandatory;
// stale, unknown, and wrong-session responses are rejected without fallback.
func (h *sessionHandle) Respond(ctx context.Context, response InteractionResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if response.InteractionID == "" {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: "respond"}
	}
	return h.driver.Respond(response)
}

func (h *sessionHandle) Status(ctx context.Context) (SessionStatus, error) {
	if err := ctx.Err(); err != nil {
		return SessionStatus{}, err
	}
	return h.driver.Status(), nil
}

// UpdateTitle durably changes the session snapshot and publishes the canonical
// ordered title transition. Callers must not synthesize title events.
func (h *sessionHandle) UpdateTitle(ctx context.Context, title string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.driver.UpdateTitle(ctx, title)
}

func sessionUnsupported(sessionID string, operation SessionOperation) error {
	return fmt.Errorf("%w: %w", &SessionError{Kind: SessionErrorUnsupported, SessionID: sessionID, Operation: operation}, ErrUnsupported)
}

func (h *sessionHandle) Skills(ctx context.Context) ([]skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Read-only query: an agent without a skills toolset simply has no skills.
	// Typed unsupported is reserved for the mutating fork operations.
	st := agentSkillsToolset(h.runtime.resolveSessionAgent(h.driver.session()))
	if st == nil {
		return nil, nil
	}
	return st.Skills(), nil
}

func (h *sessionHandle) ResolveSkillCommand(ctx context.Context, input string) (string, error) {
	// Called on every submit by App.ResolveInput: "not a skill command" is the
	// normal answer for agents without skills, never an unsupported error.
	if !strings.HasPrefix(input, "/") {
		return "", nil
	}
	st := agentSkillsToolset(h.runtime.resolveSessionAgent(h.driver.session()))
	if st == nil {
		return "", nil
	}
	name, arg, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	availableSkills, err := h.Skills(ctx)
	if err != nil {
		return "", err
	}
	for _, skill := range availableSkills {
		if skill.Name != name || skill.IsFork() {
			continue
		}
		content, err := st.ReadSkillContent(ctx, name, tools.NopRuntime{})
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(arg) != "" {
			return fmt.Sprintf("Use the following skill.\n\nUser's request: %s\n\n<skill name=%q>\n%s\n</skill>", strings.TrimSpace(arg), name, content), nil
		}
		return fmt.Sprintf("Use the following skill.\n\n<skill name=%q>\n%s\n</skill>", name, content), nil
	}
	return "", nil
}

func (h *sessionHandle) RunSkillFork(ctx context.Context, args skillstool.RunSkillArgs, sink EventSink) (*tools.ToolCallResult, error) {
	if !h.Metadata().Capabilities.ForkSkills {
		return nil, sessionUnsupported(h.sessionID, SessionOperationRunSkill)
	}
	journalSink := EventSinkFunc(func(event Event) {
		h.driver.events.Publish(h.sessionID, event)
		if sink != nil {
			sink.Emit(event)
		}
	})
	return h.runtime.RunSkillFork(ctx, h.driver.session(), args, journalSink)
}

func (h *sessionHandle) StartSkillFork(ctx context.Context, operationID string, args skillstool.RunSkillArgs) error {
	if !h.Metadata().Capabilities.ForkSkills {
		return sessionUnsupported(h.sessionID, SessionOperationRunSkill)
	}
	if operationID == "" {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: "run_skill", Reason: SessionErrorReasonBusy}
	}
	h.driver.mu.Lock()
	if admissionErr := h.driver.admitLocked(SessionOperationRunSkill); admissionErr != nil {
		h.driver.mu.Unlock()
		return admissionErr
	}
	operationCtx, cancel := context.WithCancel(ctx)
	h.driver.skillGeneration++
	generation := h.driver.skillGeneration
	h.driver.skillOperationID = operationID
	h.driver.skillCancel = cancel
	h.driver.wg.Add(1)
	h.driver.mu.Unlock()
	h.PublishOperation(SkillOperation(h.sessionID, h.agentName, operationID, args.Name, "accepted", ""))
	go func() {
		defer h.driver.wg.Done()
		defer cancel()
		result, err := h.RunSkillFork(operationCtx, args, nil)
		failure := ""
		switch {
		case err != nil:
			failure = err.Error()
		case result == nil:
			failure = "skill returned no result"
		case result.IsError:
			failure = result.Output
		}
		status := "completed"
		if failure != "" {
			status = "failed"
		}
		func() {
			h.driver.mu.Lock()
			defer h.driver.mu.Unlock()
			publish := !h.driver.stopped && h.driver.skillGeneration == generation && h.driver.skillOperationID == operationID
			if publish {
				// Publish while reservation remains held. Event hub publication does
				// not acquire the driver lock, so no admission can enter between the
				// terminal boundary and reservation release.
				h.driver.events.Publish(h.sessionID, SkillOperation(h.sessionID, h.agentName, operationID, args.Name, status, failure))
				h.driver.skillOperationID = ""
				h.driver.skillCancel = nil
			}
		}()
	}()
	return nil
}

// PublishOperation records a session-owned asynchronous operation in the same
// canonical journal observed by local and HTTP clients.
func (h *sessionHandle) PublishOperation(event Event) {
	if event != nil {
		h.driver.events.Publish(h.sessionID, event)
	}
}

func (h *sessionHandle) TogglePause(ctx context.Context) (bool, error) {
	return h.driver.TogglePause(ctx)
}

func (h *sessionHandle) Snapshot(ctx context.Context) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return h.driver.session().Clone(), nil
}

// Compact requests manual compaction through the session driver's serialized
// execution path. Active sessions reuse the live-session boundary queue; idle
// sessions are claimed by the driver until the standalone compaction completes.
func (h *sessionHandle) Compact(ctx context.Context, additionalPrompt string, sink EventSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	journalSink := EventSinkFunc(func(event Event) {
		h.driver.events.Publish(h.sessionID, event)
		if sink != nil {
			sink.Emit(event)
		}
	})
	return h.driver.compact(ctx, additionalPrompt, journalSink)
}

func (h *sessionHandle) CompactTarget(ctx context.Context, sessionID, additionalPrompt string, sink EventSink) error {
	if sessionID == h.sessionID {
		return h.Compact(ctx, additionalPrompt, sink)
	}
	if !h.sameSessionRoot(sessionID) {
		return &SessionError{Kind: SessionErrorWrongSession, SessionID: h.sessionID, Operation: "compact_target"}
	}
	target, err := h.runtime.SessionByID(sessionID)
	if err != nil {
		return err
	}
	compactor := target
	return compactor.Compact(ctx, additionalPrompt, sink)
}

func (h *sessionHandle) sameSessionRoot(targetID string) bool {
	rootOf := func(id string) (string, bool) {
		seen := map[string]struct{}{}
		for id != "" {
			if _, duplicate := seen[id]; duplicate {
				return "", false
			}
			seen[id] = struct{}{}
			driver, ok := h.runtime.sessionDrivers.Lookup(id)
			if !ok {
				return "", false
			}
			sess := driver.session()
			if sess == nil {
				return "", false
			}
			if sess.ParentID == "" {
				return sess.ID, true
			}
			id = sess.ParentID
		}
		return "", false
	}
	sourceRoot, sourceOK := rootOf(h.sessionID)
	targetRoot, targetOK := rootOf(targetID)
	return sourceOK && targetOK && sourceRoot == targetRoot
}

func (h *sessionHandle) ContextBreakdown(ctx context.Context) (*ContextBreakdown, error) {
	return h.runtime.ContextBreakdown(h.driver.scopeModels(ctx), h.driver.session())
}

func (h *sessionHandle) LiveSessions(ctx context.Context) ([]LiveSession, error) {
	return h.runtime.LiveSessions(h.driver.scopeModels(ctx), h.driver.session()), nil
}

func (h *sessionHandle) SetStarred(ctx context.Context, starred bool) error {
	return h.driver.SetStarred(ctx, starred)
}

func (h *sessionHandle) RemoveAttachment(ctx context.Context, path string) error {
	return h.driver.RemoveAttachment(ctx, path)
}

// AvailableModels returns choices decorated for the pinned agent's configured
// default. Current/session-history decoration remains an App concern.
func (h *sessionHandle) AvailableModels(ctx context.Context) []ModelChoice {
	return h.runtime.availableModels(ctx, h.agentName)
}

// SetModel applies an override to the pinned agent without consulting the
// runtime-global current-agent selection.
func (h *sessionHandle) SetModel(ctx context.Context, modelRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !h.runtime.SupportsModelSwitching() {
		return sessionUnsupported(h.sessionID, SessionOperationSetModel)
	}
	providers, err := h.runtime.resolveModelProviders(ctx, h.agentName, modelRef)
	if err != nil {
		return err
	}
	return h.driver.SetModelOverride(ctx, h.agentName, modelRef, providers)
}

// RefreshModelsCatalog refreshes the local runtime's shared model catalog.
func (h *sessionHandle) RefreshModelsCatalog(ctx context.Context) error {
	if !h.runtime.SupportsModelSwitching() {
		return sessionUnsupported(h.sessionID, SessionOperationRefreshModels)
	}
	if err := h.runtime.RefreshModelsCatalog(ctx); err != nil {
		return err
	}
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	h.metadataInitialized = false
	return nil
}

func (h *sessionHandle) ThinkingLevels(ctx context.Context) []effort.Level {
	_, models := h.driver.ModelSnapshot()
	levels, _, err := h.runtime.resolveThinkingLevelsForModels(ctx, models)
	if err != nil {
		return nil
	}
	return levels
}

func (h *sessionHandle) CurrentThinkingLevel(ctx context.Context) effort.Level {
	_, models := h.driver.ModelSnapshot()
	_, current, err := h.runtime.resolveThinkingLevelsForModels(ctx, models)
	if err != nil {
		return ""
	}
	return current
}

// CycleThinkingLevel changes the pinned agent through the owning session
// session handle, preserving session identity while the runtime recreates the
// effective providers.
func (h *sessionHandle) CycleThinkingLevel(ctx context.Context) (effort.Level, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return h.applyThinkingLevel(ctx, func(supported []effort.Level, current effort.Level) (effort.Level, error) {
		return effort.NextSupportedLevel(supported, effort.Clamp(supported, current)), nil
	})
}

// SetThinkingLevel applies a concrete level to the pinned agent through the
// owning session handle.
func (h *sessionHandle) SetThinkingLevel(ctx context.Context, level effort.Level) (effort.Level, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return h.applyThinkingLevel(ctx, func(supported []effort.Level, _ effort.Level) (effort.Level, error) {
		if !slices.Contains(supported, level) {
			return "", fmt.Errorf("thinking level %q is not supported by this model (supported: %s)", level, levelNames(supported))
		}
		return level, nil
	})
}

// applyThinkingLevel resolves the levels the session's current model supports,
// lets pick choose one, and re-creates the session's providers with it. It
// works from a single model snapshot so a concurrent model change landing
// between resolution and application can never make it validate against one
// model and apply to another.
func (h *sessionHandle) applyThinkingLevel(ctx context.Context, pick func([]effort.Level, effort.Level) (effort.Level, error)) (effort.Level, error) {
	if !h.runtime.SupportsModelSwitching() {
		return "", sessionUnsupported(h.sessionID, SessionOperationThinkingLevel)
	}
	if _, err := h.runtime.team.Agent(h.agentName); err != nil {
		return "", fmt.Errorf("agent not found: %w", err)
	}
	ref, models := h.driver.ModelSnapshot()
	supported, current, err := h.runtime.resolveThinkingLevelsForModels(ctx, models)
	if err != nil {
		return "", err
	}
	next, err := pick(supported, current)
	if err != nil {
		return "", err
	}
	updated := make([]provider.Provider, 0, len(models))
	for _, model := range models {
		baseCfg := model.BaseConfig().ModelConfig
		cfg := baseCfg.Clone()
		cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: string(next)}
		p, err := h.runtime.createProviderFromConfig(ctx, cfg)
		if err != nil {
			return "", err
		}
		updated = append(updated, p)
	}
	if err := h.driver.SetModelOverride(ctx, h.agentName, ref, updated); err != nil {
		return "", err
	}
	return next, nil
}

// EmitPinnedAgentInfo re-emits the agent/team info for the pinned agent with
// this session's model binding in scope, so a /model or thinking-level change
// made on this session is what the sidebar shows — not the agent's default.
func (h *sessionHandle) EmitPinnedAgentInfo(ctx context.Context, sink EventSink) {
	a, err := h.runtime.team.Agent(h.agentName)
	if err != nil || a == nil {
		return
	}
	h.runtime.emitAgentAndTeamInfo(h.driver.scopeModels(ctx), a, func(event Event) bool {
		if ctx.Err() != nil {
			return false
		}
		sink.Emit(event)
		return true
	})
}

// Cancel requests cancellation of the active generation. The session remains
// resumable and accepted queued input is promoted with its immutable request.
func (h *sessionHandle) Cancel(ctx context.Context, turnID string) (CancelResult, error) {
	if err := ctx.Err(); err != nil {
		return CancelResult{}, err
	}
	outcome, err := h.driver.cancelTurn(ctx, turnID)
	if err != nil {
		return CancelResult{}, err
	}
	return CancelResult{SessionID: h.sessionID, TurnID: turnID, Outcome: outcome}, nil
}

// Observe atomically registers replay/tail delivery before cloning the
// transcript. Transcript positions let consumers discard any overlap.
func (h *sessionHandle) Observe(ctx context.Context, options ObserveOptions) (Observation, error) {
	if options.Tree {
		return h.observeTree(ctx, options)
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	buffer := options.Buffer
	if buffer <= 0 {
		buffer = defaultEventChannelCapacity
	}
	since := options.Since
	if since != nil && options.SinceEpoch != h.driver.events.epoch {
		since = nil
	}
	observed := h.driver.observe(since, buffer)
	if observed.live == nil || observed.cancel == nil {
		return Observation{}, &SessionError{Kind: SessionErrorStopped, SessionID: h.sessionID, Operation: "observe"}
	}
	out := make(chan SessionEvent, buffer)
	obsCtx, cancelCtx := context.WithCancel(ctx)
	var once sync.Once
	cancel := func() { once.Do(func() { cancelCtx(); observed.cancel() }) }
	go func() {
		defer close(out)
		defer observed.cancel()
		for {
			select {
			case <-obsCtx.Done():
				return
			case item, ok := <-observed.live:
				if !ok {
					return
				}
				envelope := sessionEnvelope(h.sessionID, item)
				select {
				case out <- envelope:
				case <-obsCtx.Done():
					return
				}
			}
		}
	}()
	replay := make([]SessionEvent, len(observed.seed))
	for i, item := range observed.seed {
		replay[i] = sessionEnvelope(h.sessionID, item)
	}
	return Observation{
		Initial: []SessionSnapshot{{Epoch: h.driver.events.epoch, Session: observed.session, Status: observed.status, Interactions: observed.interactions, PendingInputs: observed.pendingInputs, Cursor: observed.cursor, TranscriptPosition: observed.position}},
		Replay:  replay, Events: out, Cancel: cancel,
	}, nil
}

func sessionEnvelope(sessionID string, item SequencedSessionEvent) SessionEvent {
	position := -1
	switch event := item.Event.(type) {
	case *PendingUserMessageAcceptedEvent:
		position = event.SessionPosition
	case *PendingUserMessageEditedEvent:
		position = event.SessionPosition
	case *PendingUserMessagePromotedEvent:
		position = event.SessionPosition
	case *MessageAddedEvent:
		position = event.SessionPosition
	case *UserMessageEvent:
		position = event.SessionPosition
	}
	return SessionEvent{Epoch: item.Epoch, Version: 1, SessionID: sessionID, TurnID: item.RequestID, InteractionID: item.InteractionID, Sequence: item.Sequence, TranscriptPosition: position, Event: item.Event, Gap: item.Gap, FirstAvailable: item.FirstAvailable}
}

// DeleteSession cascades through the topology manager, driver/interactions,
// observers, session row, and persisted topology. It is idempotent.
func (r *LocalRuntime) DeleteSession(ctx context.Context, sessionID string) error {
	// The driver registry owns the admission fence and observer finalization.
	// Runtime topology and durable state are cleared only after that completes.
	if err := r.sessionDrivers.Delete(ctx, sessionID); err != nil {
		return err
	}
	if err := r.subagents.deleteSession(ctx, sessionID); err != nil {
		return err
	}
	r.interactions.deleteSession(sessionID)
	r.sessionEvents.Delete(sessionID)
	if r.sessionStore != nil {
		if err := r.sessionStore.DeleteSession(ctx, sessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
			return err
		}
	}
	return nil
}

// ReleaseSession tears down a retryable/ephemeral session instance without
// deleting durable session state or making the stable ID final. Explicit
// DeleteSession remains the user-facing final operation.
func (r *LocalRuntime) ReleaseSession(ctx context.Context, sessionID string) error {
	return r.sessionDrivers.Release(ctx, sessionID)
}

// IsLocalSessionHandle reports whether handle is backed by this process.
func IsLocalSessionHandle(handle SessionHandle) bool {
	_, ok := handle.(*sessionHandle)
	return ok
}

// ReplaceSettledSession updates a local settled handle from a durable snapshot.
func ReplaceSettledSession(handle SessionHandle, sess *session.Session) bool {
	local, ok := handle.(*sessionHandle)
	return ok && local.ReplaceSettledSession(sess)
}

func (h *sessionHandle) ReplaceSettledSession(sess *session.Session) bool {
	return sess != nil && sess.ID == h.sessionID && h.runtime.sessionDrivers.ReplaceSettledSession(h.sessionID, h.driver, sess)
}

func (h *sessionHandle) Release(ctx context.Context) error {
	if !h.driver.Settled() {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: "release_active"}
	}
	return h.runtime.sessionDrivers.ReleaseDriver(ctx, h.sessionID, h.driver)
}

func (r *LocalRuntime) Drive(ctx context.Context, sess *session.Session) <-chan Event {
	d, err := r.sessionDrivers.GetInitialized(ctx, sess)
	if err != nil {
		out := make(chan Event, 1)
		out <- Error(err.Error())
		close(out)
		return out
	}
	return d.Drive(ctx, sess)
}

func sessionInputID(sessionID, requestID string) (string, error) {
	if requestID == "" {
		return newSessionRequestID()
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + requestID))
	return hex.EncodeToString(sum[:]), nil
}

func (h *sessionHandle) StopSubtree(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.driver.session().ParentID == "" {
		return h.stopRootTree(ctx)
	}
	h.runtime.subagents.mu.Lock()
	var nodeID subagent.NodeID
	for id, rec := range h.runtime.subagents.children {
		if rec.sessionID == h.sessionID {
			nodeID = id
			break
		}
	}
	h.runtime.subagents.mu.Unlock()
	if nodeID == "" {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: h.sessionID, Operation: "stop_subtree"}
	}
	_, err := h.runtime.subagents.stopChildContext(ctx, h.driver.session().ParentID, nodeID)
	return err
}
