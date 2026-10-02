package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Session catalog and cold session resolution: which persisted rows are
// attachable through which source registry, durable-tree validation for child
// sessions, and restoring a session (and its swarm) on first access.

type sessionTreeMember struct {
	agent    string
	parentID string
	count    int
}

type sessionTreeValidation struct {
	members map[string]sessionTreeMember
	err     error
}

type sessionCatalogIndex struct {
	byID  map[string]*session.Session
	trees map[string]sessionTreeValidation
}

func newSessionCatalogIndex(sessions []*session.Session) *sessionCatalogIndex {
	index := &sessionCatalogIndex{byID: make(map[string]*session.Session, len(sessions)), trees: make(map[string]sessionTreeValidation)}
	for _, sess := range sessions {
		index.byID[sess.ID] = sess
	}
	return index
}

func (i *sessionCatalogIndex) validateChild(ctx context.Context, registry runtime.SessionRuntime, sess, root *session.Session) error {
	inspector, ok := registry.(runtime.TreeInspector)
	if !ok {
		return errors.New("runtime cannot inspect durable child tree")
	}
	validation, loaded := i.trees[root.ID]
	if !loaded {
		snapshot, err := inspector.InspectSessionTree(ctx, root.ID)
		if err != nil {
			validation.err = fmt.Errorf("durable session tree unavailable: %w", err)
		} else {
			validation = validateSessionTree(root, snapshot)
		}
		i.trees[root.ID] = validation
	}
	if validation.err != nil {
		return validation.err
	}
	member, exists := validation.members[sess.ID]
	if !exists {
		return errors.New("session is absent from durable session tree")
	}
	if member.count != 1 {
		return fmt.Errorf("session appears %d times in durable session tree", member.count)
	}
	if member.parentID != sess.ParentID {
		return errors.New("durable tree parent session does not match persisted parent_id")
	}
	persistedAgent := sess.AgentName
	if persistedAgent == "" {
		persistedAgent = sess.AttributesSnapshot()[sessionAgentAttribute]
	}
	if persistedAgent == "" || member.agent != persistedAgent {
		return errors.New("durable session tree agent does not match persisted agent configuration")
	}
	return nil
}

func validateSessionTree(root *session.Session, snapshot *subagent.Snapshot) sessionTreeValidation {
	out := sessionTreeValidation{members: make(map[string]sessionTreeMember)}
	if root == nil {
		out.err = errors.New("persisted root session is missing")
		return out
	}
	rootSessionID := root.ID
	if snapshot == nil {
		out.err = errors.New("durable session tree is missing")
		return out
	}
	expectedRoot := subagent.SessionRootID(rootSessionID)
	if snapshot.Root != expectedRoot {
		out.err = errors.New("durable session tree root id does not match persisted root session")
		return out
	}
	if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].Node.ID != snapshot.Root || snapshot.Nodes[0].Node.Parent != "" {
		out.err = errors.New("durable session tree root is malformed")
		return out
	}
	rootAgent := root.AgentName
	if rootAgent == "" {
		rootAgent = root.AttributesSnapshot()[sessionAgentAttribute]
	}
	if rootAgent == "" || snapshot.Nodes[0].Node.Agent != rootAgent {
		out.err = errors.New("durable session tree root agent does not match persisted root configuration")
		return out
	}
	seenNodes := make(map[subagent.NodeID]struct{})
	var walk func(subagent.NodeSnapshot, subagent.NodeID, string)
	walk = func(node subagent.NodeSnapshot, expectedParent subagent.NodeID, parentSessionID string) {
		if out.err != nil {
			return
		}
		if node.Node.ID == "" || node.Node.Agent == "" || node.Node.Parent != expectedParent {
			out.err = errors.New("durable session tree node is malformed")
			return
		}
		if _, duplicate := seenNodes[node.Node.ID]; duplicate {
			out.err = errors.New("durable session tree contains duplicate node id")
			return
		}
		seenNodes[node.Node.ID] = struct{}{}
		nextParent := parentSessionID
		if node.Node.SessionID != "" {
			member := out.members[node.Node.SessionID]
			member.agent, member.parentID, member.count = node.Node.Agent, parentSessionID, member.count+1
			out.members[node.Node.SessionID] = member
			nextParent = node.Node.SessionID
		}
		for _, child := range node.Children {
			walk(child, node.Node.ID, nextParent)
		}
	}
	walk(snapshot.Nodes[0], "", rootSessionID)
	return out
}

func catalogEntryFromSession(sess *session.Session) sessionResourceDTO {
	clone := sess.Clone()
	updated := clone.CreatedAt
	activity := "idle"
	pending := 0
	lastError := ""
	for _, item := range clone.Messages {
		if item.Message != nil {
			if item.Message.Pending {
				pending++
			}
			if at, err := time.Parse(time.RFC3339, item.Message.Message.CreatedAt); err == nil && at.After(updated) {
				updated = at
			}
			activity = string(item.Message.Message.Role)
		}
		if item.Error != nil {
			lastError = item.Error.Message
			if at, err := time.Parse(time.RFC3339, item.Error.CreatedAt); err == nil && at.After(updated) {
				updated = at
			}
		}
	}
	inputTokens, outputTokens := clone.Usage()
	entry := sessionResourceDTO{
		SessionID: clone.ID, ParentID: clone.ParentID, Source: clone.AttributesSnapshot()[sessionSourceAttribute],
		AgentName: clone.AgentName, Title: clone.TitleSnapshot(), CreatedAt: clone.CreatedAt.UTC().Format(time.RFC3339Nano),
		Messages: clone.GetAllMessages(), ToolsApproved: clone.ToolsApproved, SafetyPolicy: clone.SafetyPolicy,
		Starred:     clone.Starred,
		InputTokens: inputTokens, OutputTokens: outputTokens, WorkingDir: clone.WorkingDir, Permissions: clone.ClonePermissions(),
		Activity: activity, UpdatedAt: updated.UTC().Format(time.RFC3339Nano), LastError: lastError,
	}
	if pending > 0 {
		entry.Pending = &pending
	}
	if entry.AgentName == "" {
		entry.AgentName = clone.AttributesSnapshot()[sessionAgentAttribute]
	}
	return entry
}

func (s *Server) getCanonicalSession(c echo.Context) error {
	if views, present := c.QueryParams()["view"]; present {
		if len(views) != 1 || views[0] != "prepare-info" {
			return sessionRequestError("unknown session view")
		}
		info, err := s.sm.ConfirmedSessionViewInfo(c.Request().Context(), c.Param("id"))
		if err != nil {
			return sessionHTTPError(err)
		}
		return c.JSON(http.StatusOK, struct {
			Version int                             `json:"version"`
			View    string                          `json:"view"`
			Info    runtime.PreparedSessionViewInfo `json:"info"`
		}{Version: api.SessionAPIVersion, View: "prepare-info", Info: info})
	}
	sess, err := s.sm.GetSession(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	entry := catalogEntryFromSession(sess)
	if handle := s.sm.loadedSession(sess.ID); handle != nil {
		entry.Loaded, entry.Loadable, entry.Attachable = true, true, true
		entry.AgentName = handle.Metadata().AgentName
		if status, statusErr := handle.Status(c.Request().Context()); statusErr == nil {
			entry.StateKnown, entry.State, entry.LastError = true, status.State, status.LastError
			pending := status.Pending
			entry.Pending = &pending
		}
	} else if entry.AgentName != "" {
		entry.Loadable, entry.Attachable = true, true
	} else {
		entry.RouteError = "session is not attachable"
	}
	return c.JSON(http.StatusOK, entry)
}

func (s *Server) canonicalSessionSnapshot(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	observation, err := handle.Observe(c.Request().Context(), runtime.ObserveOptions{})
	if err != nil {
		return sessionHTTPError(err)
	}
	observation.Cancel()
	return c.JSON(http.StatusOK, sessionSnapshot(observation.Primary()))
}

func (s *Server) sessionTodos(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	reader := handle
	items, err := reader.Todos(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, items)
}

func (s *Server) sessionTree(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	registry := s.sm.sessionRegistry
	if active, ok := s.sm.runtimeSessions.Load(handle.ID()); ok && active.registry != nil {
		registry = active.registry
	}
	inspector, ok := registry.(runtime.TreeInspector)
	if !ok {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "inspect_tree"})
	}
	snapshot, err := inspector.InspectSessionTree(c.Request().Context(), handle.ID())
	if err != nil {
		return sessionHTTPError(err)
	}
	if snapshot == nil {
		snapshot = &subagent.Snapshot{Version: subagent.SnapshotVersion}
	}
	return c.JSON(http.StatusOK, snapshot)
}

func (s *Server) createCanonicalSession(c echo.Context) error {
	var req sessionCreateRequest
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if strings.TrimSpace(req.AgentName) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "agent_name is required")
	}
	registry, source, err := s.sm.sessionRegistryForCreate(req.Source)
	if err != nil {
		return sessionHTTPError(err)
	}
	template := &session.Session{
		Title:         req.Title,
		WorkingDir:    req.WorkingDir,
		SafetyPolicy:  req.SafetyPolicy,
		ToolsApproved: req.ToolsApproved,
		Permissions:   req.Permissions,
	}
	template.SetAttribute(sessionAgentAttribute, req.AgentName)
	if source != "" {
		template.SetAttribute(sessionSourceAttribute, source)
	}
	sess, err := s.sm.prepareSession(template)
	if err != nil {
		return sessionHTTPError(err)
	}
	sess.AgentName = req.AgentName
	if sess.GetSafetyPolicy() == "" && !sess.ToolsApproved {
		// No client choice: seed the author-declared YAML default, as a fresh
		// local session would be. A default never overrides a stated policy.
		// The canonical runtime persists the session on creation.
		if defaults, ok := registry.(runtime.SafetyDefaults); ok {
			if policy := authorSafetyDefault(c.Request().Context(), defaults, sess); policy != "" {
				sess.SetSafetyPolicy(policy)
			}
		}
	}
	handle, err := s.sm.createHTTPSession(c.Request().Context(), registry, sess, runtime.SessionBinding{AgentName: req.AgentName, Model: req.Model, ParentSessionID: req.ParentSessionID})
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusCreated, sessionMetadata(handle.Metadata()))
}

func (sm *SessionManager) createHTTPSession(ctx context.Context, registry runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	if registry == nil {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sess.ID, Operation: "create_session"}
	}
	// Stamp only the detached template; the runtime owns durable publication.
	sess.AgentName = binding.AgentName
	sess.SetAttribute(sessionAgentAttribute, binding.AgentName)
	handle, err := registry.CreateSession(ctx, sess, binding)
	if err != nil {
		return nil, err
	}
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{handle: handle, registry: registry})
	sm.markReady()
	return handle, nil
}

func (sm *SessionManager) sessionRegistryForCreate(source string) (runtime.SessionRuntime, string, error) {
	if len(sm.sessionRegistries) == 0 {
		if sm.sessionRegistry == nil {
			return nil, "", &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, Operation: "create_session"}
		}
		if source != "" {
			return nil, "", &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: "source"}
		}
		return sm.sessionRegistry, "", nil
	}
	if source == "" {
		if len(sm.sessionRegistries) != 1 {
			return nil, "", &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: "source"}
		}
		for name, registry := range sm.sessionRegistries {
			return registry, name, nil
		}
	}
	registry := sm.sessionRegistries[source]
	if registry == nil {
		return nil, "", &runtime.SessionError{Kind: runtime.SessionErrorNotFound, Operation: "source"}
	}
	return registry, source, nil
}

func (s *Server) sessionByID(ctx context.Context, id string) (runtime.SessionHandle, error) {
	return s.sm.Handle(ctx, id)
}

// Handle resolves a stable session handle using the same persisted source,
// ancestry, durable-tree membership, and cold-restore validation as HTTP.
func (sm *SessionManager) Handle(ctx context.Context, id string) (runtime.SessionHandle, error) {
	if id == "" {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
	}
	if handle := sm.loadedSession(id); handle != nil {
		return handle, nil
	}
	sess, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		// Single-registry embeddings may own sessions that are intentionally not
		// represented in this server's store.
		if len(sm.sessionRegistries) == 0 && sm.sessionRegistry != nil {
			if handle, lookupErr := sm.sessionRegistry.SessionByID(id); lookupErr == nil {
				return handle, nil
			}
		}
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
	}
	if sess.ParentID == "" && sess.AttributesSnapshot()[sessionAgentAttribute] == "" {
		return nil, nonAttachableSessionError(id)
	}
	root, err := sm.sessionRestoreRoot(ctx, sess)
	if err != nil {
		return nil, err
	}
	unlock := sm.sessionRestoreLocks.lock(root.ID)
	defer unlock()

	if handle := sm.loadedSession(id); handle != nil {
		return handle, nil
	}
	registry, _, routeErr := sm.sessionRegistryForCreate(root.AttributesSnapshot()[sessionSourceAttribute])
	if routeErr != nil {
		return nil, routeErr
	}
	if sess.ParentID != "" {
		index := newSessionCatalogIndex([]*session.Session{root, sess})
		// Cold lookup has only one row and its ancestors, so build that bounded
		// index explicitly; catalog builds the same index once for every row.
		current := sess
		for current.ParentID != "" && current.ParentID != root.ID {
			parent, parentErr := sm.sessionStore.GetSession(ctx, current.ParentID)
			if parentErr != nil {
				return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "restore_parent"}
			}
			index.byID[parent.ID] = parent
			current = parent
		}
		if treeErr := index.validateChild(ctx, registry, sess, root); treeErr != nil {
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: id, Operation: "restore_tree_membership"}
		}
	}
	if handle, lookupErr := registry.SessionByID(id); lookupErr == nil {
		sm.runtimeSessions.Store(id, &activeRuntimes{handle: handle, registry: registry})
		sm.markReady()
		return handle, nil
	}
	if root.AgentName == "" {
		root.AgentName = root.AttributesSnapshot()[sessionAgentAttribute]
	}
	if root.AgentName == "" {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: root.ID, Operation: "restore_binding"}
	}
	rootSession, lookupErr := registry.SessionByID(root.ID)
	if lookupErr != nil {
		rootSession, err = sm.createHTTPSession(ctx, registry, root, runtime.SessionBinding{AgentName: root.AgentName})
		if err != nil {
			return nil, err
		}
	} else if _, ok := sm.runtimeSessions.Load(root.ID); !ok {
		sm.runtimeSessions.Store(root.ID, &activeRuntimes{handle: rootSession, registry: registry})
	}
	if id == root.ID {
		return rootSession, nil
	}
	restorer, ok := registry.(runtime.TreeRestorer)
	if !ok {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: id, Operation: "restore_child_tree"}
	}
	if err := restorer.RestoreSessionTree(ctx, root); err != nil {
		return nil, err
	}
	handle, err := registry.SessionByID(id)
	if err != nil {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "restore_child_tree"}
	}
	sm.runtimeSessions.Store(id, &activeRuntimes{handle: handle, registry: registry})
	return handle, nil
}

func (sm *SessionManager) loadedSession(id string) runtime.SessionHandle {
	if active, ok := sm.runtimeSessions.Load(id); ok && active.handle != nil {
		return active.handle
	}
	return nil
}

func (sm *SessionManager) sessionRestoreRoot(ctx context.Context, sess *session.Session) (*session.Session, error) {
	root := sess
	seen := map[string]struct{}{root.ID: {}}
	source := root.AttributesSnapshot()[sessionSourceAttribute]
	for root.ParentID != "" {
		parent, err := sm.sessionStore.GetSession(ctx, root.ParentID)
		if err != nil {
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: root.ID, Operation: "restore_parent"}
		}
		if _, duplicate := seen[parent.ID]; duplicate {
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: sess.ID, Operation: "restore_ancestry"}
		}
		seen[parent.ID] = struct{}{}
		parentSource := parent.AttributesSnapshot()[sessionSourceAttribute]
		if source != "" && parentSource != source {
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: sess.ID, Operation: "restore_source"}
		}
		source = parentSource
		root = parent
	}
	return root, nil
}
