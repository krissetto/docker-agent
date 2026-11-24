package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
	metadataProjection, projectionErr := sessionViewProjectionError(c, "prepare-info")
	if projectionErr != nil {
		return projectionErr
	}
	if views, present := c.QueryParams()["view"]; present {
		if len(views) != 1 || views[0] != "prepare-info" {
			return sessionRequestError("unknown session view")
		}
		ctx := c.Request().Context()
		started := time.Now()
		info, err := s.sm.confirmedSessionViewInfo(ctx, c.Param("id"), metadataProjection)
		s.sessionTiming.DebugContext(ctx, "Session stream timing", "phase", "prepare_view", "elapsed", time.Since(started), "metadata_only", metadataProjection, "ok", err == nil)
		if err != nil {
			return sessionHTTPError(err)
		}
		if metadataProjection {
			return c.JSON(http.StatusOK, struct {
				Version    int                             `json:"version"`
				View       string                          `json:"view"`
				Projection string                          `json:"projection"`
				Info       metadataPreparedSessionViewInfo `json:"info"`
			}{Version: api.SessionAPIVersion, View: "prepare-info", Projection: api.SessionViewMetadataProjection, Info: metadataPreparedViewInfo(info)})
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
	projection, limits, err := parseHistoryProjection(c, false, false)
	if err != nil {
		return sessionRequestError(err.Error())
	}
	handle, err := s.sm.handle(c.Request().Context(), c.Param("id"), projection == "tail")
	if err != nil {
		return sessionHTTPError(err)
	}
	if projection == "tail" {
		observation, err := handle.Observe(c.Request().Context(), runtime.ObserveOptions{HistoryTail: &limits})
		if err != nil {
			return sessionHTTPError(err)
		}
		defer observation.Cancel()
		dto := sessionSnapshotForRequest(observation.Primary(), c)
		if session.BoundedJSONSize(dto, limits.MaxBytes) > limits.MaxBytes {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, map[string]any{"error": "history_metadata_too_large"})
		}
		data, err := json.Marshal(dto)
		if err != nil {
			return sessionHTTPError(err)
		}
		if len(data) > limits.MaxBytes {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, map[string]any{"error": "history_metadata_too_large"})
		}
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, data)
	}
	observation, err := handle.Observe(c.Request().Context(), runtime.ObserveOptions{})
	if err != nil {
		return sessionHTTPError(err)
	}
	observation.Cancel()
	return c.JSON(http.StatusOK, sessionSnapshotForRequest(observation.Primary(), c))
}

func (s *Server) canonicalSessionHistory(c echo.Context) error {
	started := time.Now()
	projection, limits, err := parseHistoryProjection(c, true, false)
	if err != nil || projection != "tail" {
		return sessionRequestError("history requires projection=tail")
	}
	cursor := c.QueryParam("cursor")
	if cursor == "" {
		return sessionRequestError("cursor is required")
	}
	handle, err := s.sm.handle(c.Request().Context(), c.Param("id"), true)
	if err != nil {
		return sessionHTTPError(err)
	}
	reader, ok := handle.(runtime.SessionHistoryReader)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), runtime.SessionOperationObserve))
	}
	var body []byte
	var acceptedPage runtime.SessionSnapshot
	err = reader.ReadHistoryPageBounded(c.Request().Context(), handle.ID(), cursor, limits, func(page runtime.SessionSnapshot) bool {
		out := api.SessionHistoryPage{Projection: "tail", SessionID: handle.ID(), Epoch: page.Epoch, Cursor: page.Cursor, TranscriptPosition: page.TranscriptPosition, History: api.SessionHistory{StartPosition: page.History.StartPosition, EndPosition: page.History.BeforePosition, TotalItems: page.History.TotalItems, Truncated: page.History.HasMore, NextCursor: page.HistoryCursor}}
		out.TurnTimings, out.TurnOutcomes = page.History.TurnTimings, page.History.TurnOutcomes
		out.Items = make([]api.SessionHistoryItem, len(page.Items))
		for i, item := range page.Items {
			out.Items[i] = api.SessionHistoryItem{Position: item.Position, Item: item.Item}
		}
		out.History.OmittedRanges = page.HistoryOmittedRanges()
		if session.BoundedJSONSize(out, limits.MaxBytes) > limits.MaxBytes {
			return false
		}
		data, e := json.Marshal(out)
		if e != nil || len(data) > limits.MaxBytes {
			return false
		}
		body = data
		acceptedPage = page
		return true
	})
	if err != nil {
		return sessionHTTPError(err)
	}
	if acceptedPage.History == nil {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, map[string]any{"error": "history_metadata_too_large"})
	}
	s.sessionTiming.DebugContext(c.Request().Context(), "Session stream timing", "phase", "history_page", "elapsed", time.Since(started), "response_bytes", len(body), "items", len(acceptedPage.Items), "scanned_positions", acceptedPage.History.ScannedPositions)
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, body)
}

func parseHistoryProjection(c echo.Context, allowCursor, allowStream bool) (string, runtime.SessionHistoryLimits, error) {
	values := c.QueryParams()
	for key, vals := range values {
		if key != "projection" && key != "max_items" && key != "max_bytes" && key != "max_item_bytes" && !(allowCursor && key == "cursor") && !(allowStream && (key == "since" || key == "since_epoch" || key == "tree")) {
			return "", runtime.SessionHistoryLimits{}, fmt.Errorf("unknown query parameter %q", key)
		}
		if len(vals) > 1 {
			return "", runtime.SessionHistoryLimits{}, fmt.Errorf("duplicate query parameter %q", key)
		}
	}
	projection := c.QueryParam("projection")
	if projection == "" {
		if c.QueryParam("max_items") != "" || c.QueryParam("max_bytes") != "" || c.QueryParam("max_item_bytes") != "" {
			return "", runtime.SessionHistoryLimits{}, errors.New("history bounds require projection=tail")
		}
		return "", runtime.SessionHistoryLimits{}, nil
	}
	if projection == api.SessionViewMetadataProjection && allowStream && c.QueryParam("tree") == "true" && c.QueryParam("max_items") == "" && c.QueryParam("max_bytes") == "" && c.QueryParam("max_item_bytes") == "" {
		return projection, runtime.SessionHistoryLimits{}, nil
	}
	if projection != "tail" {
		return "", runtime.SessionHistoryLimits{}, errors.New("projection must be tail")
	}
	limits := runtime.SessionHistoryLimits{}
	var err error
	if v := c.QueryParam("max_items"); v != "" {
		limits.MaxItems, err = strconv.Atoi(v)
		if err != nil {
			return "", limits, errors.New("max_items must be an integer")
		}
	}
	if v := c.QueryParam("max_bytes"); v != "" {
		limits.MaxBytes, err = strconv.Atoi(v)
		if err != nil {
			return "", limits, errors.New("max_bytes must be an integer")
		}
	}
	if v := c.QueryParam("max_item_bytes"); v != "" {
		limits.MaxItemBytes, err = strconv.Atoi(v)
		if err != nil {
			return "", limits, errors.New("max_item_bytes must be an integer")
		}
	}
	if _, err = runtime.NormalizeHistoryLimits(limits); err != nil {
		return "", limits, err
	}
	return projection, limits, nil
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
	if strings.TrimSpace(req.AgentName) == "" && (req.AgentName != "" || req.ParentSessionID != "") {
		return echo.NewHTTPError(http.StatusBadRequest, "agent_name is required")
	}
	if req.SessionID != "" {
		if !validSessionCreateID(req.SessionID) {
			return sessionRequestError("invalid session_id")
		}
		if req.ParentSessionID != "" {
			return sessionRequestError("session_id is supported only for root sessions")
		}
		// Share the restore lock so a caller cannot race cold attachment or another
		// create into replacing a durable identity. POST create is never attach.
		unlock := s.sm.sessionRestoreLocks.lock(req.SessionID)
		defer unlock()
		if _, err := s.sm.sessionStore.GetSession(c.Request().Context(), req.SessionID); err == nil {
			return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorConflict, SessionID: req.SessionID, Operation: "create_session"})
		} else if !errors.Is(err, session.ErrNotFound) {
			return sessionHTTPError(err)
		}
		if _, loaded := s.sm.runtimeSessions.Load(req.SessionID); loaded {
			return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorConflict, SessionID: req.SessionID, Operation: "create_session"})
		}
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
	if req.AgentName != "" {
		template.SetAttribute(sessionAgentAttribute, req.AgentName)
	}
	if source != "" {
		template.SetAttribute(sessionSourceAttribute, source)
	}
	sess, err := s.sm.prepareSession(template)
	if err != nil {
		return sessionHTTPError(err)
	}
	if req.SessionID != "" {
		sess.ID = req.SessionID
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
	metadata := sessionMetadata(handle.Metadata())
	if c.QueryParam("view") == "created" {
		// Capture canonical create-time fields without opening a second observer.
		created, err := handle.Snapshot(c.Request().Context())
		if err != nil {
			return sessionHTTPError(err)
		}
		if created == nil || created.ID != handle.ID() {
			return errors.New("invalid created session snapshot")
		}
		return c.JSON(http.StatusCreated, api.SessionCreated{SessionMetadata: metadata, Session: created, ParentSessionID: created.ParentID})
	}
	return c.JSON(http.StatusCreated, metadata)
}

func (sm *SessionManager) createHTTPSession(ctx context.Context, registry runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	if registry == nil {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sess.ID, Operation: "create_session"}
	}
	// Stamp only the detached template; the runtime owns durable publication.
	if binding.AgentName != "" {
		sess.AgentName = binding.AgentName
		sess.SetAttribute(sessionAgentAttribute, binding.AgentName)
	}
	handle, err := registry.CreateSession(ctx, sess, binding)
	if err != nil {
		return nil, err
	}
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{handle: handle, registry: registry})
	sm.markReady()
	return handle, nil
}

func (sm *SessionManager) sessionRegistryForCreate(source string) (runtime.SessionRuntime, string, error) {
	sm.sourceMu.RLock()
	defer sm.sourceMu.RUnlock()
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
	return sm.handle(ctx, id, false)
}

func (sm *SessionManager) handle(ctx context.Context, id string, metadataOnly bool) (runtime.SessionHandle, error) {
	if id == "" {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
	}
	if handle := sm.loadedSession(id); handle != nil {
		return handle, nil
	}
	if _, err := session.ReadSessionMetadata(ctx, sm.sessionStore, id); err != nil {
		if sm.sourceRegistryCount() == 0 && sm.sessionRegistry != nil {
			if handle, lookupErr := sm.sessionRegistry.SessionByID(id); lookupErr == nil {
				return handle, nil
			}
		}
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
	}
	prepared, err := sm.prepareSessionView(ctx, id, metadataOnly)
	if err != nil {
		return nil, err
	}
	defer prepared.Abort()
	committed, err := prepared.Commit(ctx)
	if err != nil {
		return nil, err
	}
	sm.markReady()
	return committed.SessionHandle, nil
}

// loadedSession returns the cached handle only while its registry still
// registers the session. Capacity and idle eviction reclaim a settled driver
// without notifying the server, so a NotFound marks the entry stale and the
// caller reopens from the durable store. Any other lookup error keeps the
// entry: a transient failure must not turn a live session into a cold reopen.
// Intentionally stopped sessions remain registered and stay loaded.
func (sm *SessionManager) loadedSession(id string) runtime.SessionHandle {
	active, ok := sm.runtimeSessions.Load(id)
	if !ok || active.handle == nil {
		return nil
	}
	if active.registry == nil {
		return active.handle
	}
	fresh, err := active.registry.SessionByID(id)
	if err != nil {
		if sessionErr, ok := errors.AsType[*runtime.SessionError](err); ok && sessionErr.Kind == runtime.SessionErrorNotFound {
			return nil
		}
		return active.handle
	}
	return fresh
}

func (sm *SessionManager) sessionRestoreRoot(ctx context.Context, sess *session.Session) (*session.Session, error) {
	root := sess
	seen := map[string]struct{}{root.ID: {}}
	source := root.AttributesSnapshot()[sessionSourceAttribute]
	for root.ParentID != "" {
		parent, err := session.ReadSessionMetadata(ctx, sm.sessionStore, root.ParentID)
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

// Explicit IDs are URL path segments and durable storage keys. Keep the public
// alphabet portable; never accept encoded separators or control characters.
func validSessionCreateID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
