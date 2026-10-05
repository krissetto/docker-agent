package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

const sessionWireVersion = api.SessionAPIVersion

// SessionTransport is the first-party session-v2 HTTP transport. It is a
// borrowed registry: closing an observation never stops server-side execution.
type SessionTransport struct {
	client *Client
	source string
}

// SessionTransportOption fixes transport identity at construction.
type SessionTransportOption func(*SessionTransport)

// WithSessionTransportSource selects the serve-api source used for every session
// created through this registry. Source identity is immutable and independent
// from agent/model binding.
func WithSessionTransportSource(source string) SessionTransportOption {
	return func(runtime *SessionTransport) { runtime.source = source }
}

func NewSessionTransport(client *Client, opts ...SessionTransportOption) (*SessionTransport, error) {
	if client == nil {
		return nil, errors.New("client cannot be nil")
	}
	runtime := &SessionTransport{client: client}
	for _, opt := range opts {
		opt(runtime)
	}
	return runtime, nil
}

func (r *SessionTransport) CreateSession(ctx context.Context, sess *session.Session, binding SessionBinding) (SessionHandle, error) {
	return r.createSession(ctx, sess, binding, "")
}

func (r *SessionTransport) createSession(ctx context.Context, sess *session.Session, binding SessionBinding, desiredID string) (SessionHandle, error) {
	if sess == nil {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "create_session"}
	}
	template := sess.Clone()
	request := api.SessionCreateRequest{
		SessionID: desiredID,
		Source:    r.source, AgentName: binding.AgentName, Model: binding.Model,
		Title: template.Title, ParentSessionID: binding.ParentSessionID,
		WorkingDir: template.WorkingDir, SafetyPolicy: template.SafetyPolicy,
		ToolsApproved: template.ToolsApproved, Permissions: template.Permissions,
	}
	var metadata remoteSessionMetadata
	if err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath, request, &metadata); err != nil {
		return nil, err
	}
	if metadata.SessionID == "" {
		return nil, errors.New("session create response has empty session_id")
	}
	if desiredID != "" && metadata.SessionID != desiredID {
		return nil, errors.New("session create response differs from requested session_id")
	}
	return &remoteSession{runtime: r, sessionID: metadata.SessionID, metadata: metadata.runtime()}, nil
}

func (r *SessionTransport) SessionByID(id string) (SessionHandle, error) {
	if id == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "lookup"}
	}
	return &remoteSession{runtime: r, sessionID: id, metadata: SessionMetadata{SessionID: id}}, nil
}

func (r *SessionTransport) SwitchAgent(ctx context.Context, sessionID, targetAgent string) (SessionHandle, *session.Session, error) {
	var response struct {
		Metadata remoteSessionMetadata `json:"metadata"`
		Session  *session.Session      `json:"session"`
	}
	if err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/switch-agent", struct {
		AgentName string `json:"agent_name"`
	}{targetAgent}, &response); err != nil {
		return nil, nil, err
	}
	if response.Session == nil {
		return nil, nil, errors.New("switch-agent response has no session")
	}
	return &remoteSession{runtime: r, sessionID: response.Metadata.SessionID, metadata: response.Metadata.runtime()}, response.Session, nil
}

func (r *SessionTransport) DeleteSession(ctx context.Context, id string) error {
	return r.client.sessionJSON(ctx, http.MethodDelete, api.SessionAPIPath+"/"+url.PathEscape(id), nil, nil)
}

func (r *SessionTransport) ListSessions(ctx context.Context) ([]SessionCatalogEntry, error) {
	query := url.Values{"limit": {strconv.Itoa(api.SessionCatalogDefaultLimit)}}
	out := make([]SessionCatalogEntry, 0)
	seenIDs, seenCursors := map[string]bool{}, map[string]bool{}
	for {
		var catalog api.SessionCatalog[SessionState]
		if err := r.client.sessionJSON(ctx, http.MethodGet, api.SessionAPIPath+"?"+query.Encode(), nil, &catalog); err != nil {
			return nil, err
		}
		if catalog.Version != sessionWireVersion {
			return nil, fmt.Errorf("unsupported session catalog version %d (expected %d)", catalog.Version, sessionWireVersion)
		}
		for _, row := range catalog.Sessions {
			if row.SessionID == "" || seenIDs[row.SessionID] {
				return nil, errors.New("invalid or duplicate session catalog identity")
			}
			seenIDs[row.SessionID] = true
			var createdAt time.Time
			if row.CreatedAt != "" {
				var err error
				createdAt, err = time.Parse(time.RFC3339Nano, row.CreatedAt)
				if err != nil {
					return nil, fmt.Errorf("invalid session catalog creation time: %w", err)
				}
			}
			out = append(out, SessionCatalogEntry{SessionID: row.SessionID, Title: row.Title, AgentName: row.AgentName, UpdatedAt: row.UpdatedAt, CreatedAt: createdAt, Starred: row.Starred, Loadable: row.Loadable, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir})
		}
		if err := advanceSessionCatalog(query, seenCursors, catalog.NextCursor, len(catalog.Sessions)); err != nil {
			return nil, err
		}
		if catalog.NextCursor == "" {
			return out, nil
		}
	}
}

// ListSessionSummaries explicitly negotiates the metadata view. An older
// server's transcript catalog is not accepted as a silent fallback. All pages
// are required: any later failure discards the incomplete listing.
func (r *SessionTransport) ListSessionSummaries(ctx context.Context, options SessionSummaryOptions) ([]SessionSummaryEntry, error) {
	query := url.Values{"view": {"summary"}, "include_children": {strconv.FormatBool(options.IncludeChildren)}, "limit": {strconv.Itoa(api.SessionCatalogDefaultLimit)}}
	var out []SessionSummaryEntry
	seenIDs, seenCursors := map[string]bool{}, map[string]bool{}
	for {
		var catalog api.SessionSummaryCatalog[SessionSummaryEntry]
		if err := r.client.sessionJSON(ctx, http.MethodGet, api.SessionAPIPath+"?"+query.Encode(), nil, &catalog); err != nil {
			return nil, err
		}
		if catalog.Version != sessionWireVersion || catalog.View != "summary" {
			return nil, UnsupportedSessionOperation("", "session_summaries")
		}
		for _, row := range catalog.Sessions {
			if row.SessionID == "" {
				return nil, errors.New("session summary is missing canonical identity")
			}
			if seenIDs[row.SessionID] {
				return nil, errors.New("duplicate session summary identity")
			}
			seenIDs[row.SessionID] = true
			if !options.IncludeChildren && row.ParentID != "" {
				return nil, errors.New("session summary exceeds requested scope")
			}
		}
		out = append(out, catalog.Sessions...)
		if err := advanceSessionCatalog(query, seenCursors, catalog.NextCursor, len(catalog.Sessions)); err != nil {
			return nil, err
		}
		if catalog.NextCursor == "" {
			return out, nil
		}
	}
}

// Cursors are opaque base64url tokens. Validate only their transport shape and
// progress; their contents and ordering belong to the server.
func advanceSessionCatalog(query url.Values, seen map[string]bool, cursor string, rows int) error {
	if cursor == "" {
		return nil
	}
	if rows == 0 || len(cursor) > 2048 || strings.IndexFunc(cursor, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) >= 0 {
		return errors.New("invalid session catalog cursor or empty continuation page")
	}
	if seen[cursor] {
		return errors.New("repeated session catalog cursor")
	}
	seen[cursor] = true
	query.Set("cursor", cursor)
	return nil
}

func (r *SessionTransport) LoadSession(ctx context.Context, id string) (SessionHandle, *session.Session, error) {
	handle, err := r.SessionByID(id)
	if err != nil {
		return nil, nil, err
	}
	if hydrator, ok := handle.(Hydrator); ok {
		if err := hydrator.Hydrate(ctx); err != nil {
			var sessionErr *SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionErrorUnsupported {
				return nil, nil, err
			}
		}
	}
	observation, err := handle.Observe(ctx, ObserveOptions{})
	if err != nil {
		return nil, nil, err
	}
	defer observation.Cancel()
	return handle, observation.Primary().Session, nil
}

type remoteSession struct {
	runtime   *SessionTransport
	sessionID string
	mu        sync.RWMutex
	metadata  SessionMetadata
}

func (s *remoteSession) ID() string { return s.sessionID }
func (s *remoteSession) AgentName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadata.AgentName
}

func (s *remoteSession) Metadata() SessionMetadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata := s.metadata
	metadata.ThinkingLevels = slices.Clone(metadata.ThinkingLevels)
	metadata.Capabilities.AvailableModels = slices.Clone(metadata.Capabilities.AvailableModels)
	return metadata
}

func (s *remoteSession) Submit(ctx context.Context, input TurnInput) (Submission, error) {
	return s.input(ctx, "submit", input)
}

func (s *remoteSession) Steer(ctx context.Context, input TurnInput) (Submission, error) {
	return s.input(ctx, "steer", input)
}

func (s *remoteSession) input(ctx context.Context, operation string, input TurnInput) (Submission, error) {
	request := api.SessionInputRequest{Content: input.Content, MultiContent: input.MultiContent, Mode: operation, RequestID: input.RequestID}
	var out api.SessionSubmission[SubmissionDisposition]
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("messages"), request, &out)
	if err == nil && (out.SessionID != s.ID() || out.TurnID == "") {
		return Submission{}, fmt.Errorf("invalid session submission identity: session=%q turn=%q", out.SessionID, out.TurnID)
	}
	return Submission(out), err
}

func (s *remoteSession) Retry(ctx context.Context) (Submission, error) {
	var out api.SessionSubmission[SubmissionDisposition]
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("retry"), struct{}{}, &out)
	if err == nil && (out.SessionID != s.ID() || out.TurnID == "") {
		return Submission{}, fmt.Errorf("invalid session retry identity: session=%q turn=%q", out.SessionID, out.TurnID)
	}
	return Submission(out), err
}

func (s *remoteSession) Snapshot(ctx context.Context) (*session.Session, error) {
	var raw remoteSessionSnapshot
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("snapshot"), nil, &raw); err != nil {
		return nil, err
	}
	decoded, err := s.runtime.client.decodeSessionSnapshot(raw)
	if err != nil {
		return nil, err
	}
	return decoded.Session, nil
}

func (s *remoteSession) Hydrate(ctx context.Context) error {
	_, err := s.Status(ctx)
	return err
}

func (s *remoteSession) Status(ctx context.Context) (SessionStatus, error) {
	var out struct {
		Metadata remoteSessionMetadata           `json:"metadata"`
		Status   api.SessionStatus[SessionState] `json:"status"`
	}
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("status"), nil, &out); err != nil {
		return SessionStatus{}, err
	}
	if out.Status.SessionID != s.ID() || out.Metadata.SessionID != s.ID() {
		return SessionStatus{}, fmt.Errorf("invalid session status identity for %q", s.ID())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadata = out.Metadata.runtime()
	return SessionStatus(out.Status), nil
}

func (s *remoteSession) Respond(ctx context.Context, response InteractionResponse) error {
	request := api.SessionResponseRequest[InteractionKind]{InteractionID: response.InteractionID, Kind: response.Kind, Confirmation: string(response.Resume.Type), Reason: response.Resume.Reason, ToolName: response.Resume.ToolName, ElicitationID: response.ElicitationID, Action: string(response.Elicitation.Action), Content: response.Elicitation.Content, ClientID: response.ClientID}
	return s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("responses"), request, nil)
}

func (s *remoteSession) Edit(ctx context.Context, edit SessionEdit) (*session.Session, error) {
	var updated session.Session
	if err := s.runtime.client.sessionJSON(ctx, http.MethodPatch, api.SessionAPIPath+"/"+url.PathEscape(s.ID()), edit, &updated); err != nil {
		return nil, err
	}
	if updated.ID != s.ID() {
		return nil, errors.New("invalid session edit identity")
	}
	return &updated, nil
}

func (s *remoteSession) UpdateTitle(ctx context.Context, title string) error {
	return s.runtime.client.sessionJSON(ctx, http.MethodPatch, s.endpoint("title"), struct {
		Title string `json:"title"`
	}{title}, nil)
}

func (s *remoteSession) Cancel(ctx context.Context, turnID string) (CancelResult, error) {
	var out CancelResult
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("cancel"), api.SessionCancelRequest{TurnID: turnID}, &out)
	if err == nil && out.SessionID != s.ID() {
		return CancelResult{}, fmt.Errorf("invalid session cancel identity %q", out.SessionID)
	}
	return out, err
}

func (s *remoteSession) StopSubtree(ctx context.Context) error {
	if !s.Metadata().Capabilities.StopSubtree {
		return sessionUnsupported(s.ID(), "stop_subtree")
	}
	return s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("stop-subtree"), nil, nil)
}

func (s *remoteSession) Release(context.Context) error { return nil }
func (s *remoteSession) Observe(ctx context.Context, options ObserveOptions) (Observation, error) {
	return s.runtime.client.attachSession(ctx, s.ID(), options)
}

func (s *remoteSession) Compact(ctx context.Context, additionalPrompt string, _ EventSink) error {
	if !s.Metadata().Capabilities.Compaction {
		return sessionUnsupported(s.ID(), SessionOperationCompact)
	}
	return s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("compact"), struct {
		AdditionalPrompt string `json:"additional_prompt,omitempty"`
	}{additionalPrompt}, nil)
}

func (s *remoteSession) CompactTarget(ctx context.Context, sessionID, additionalPrompt string, _ EventSink) error {
	if !s.Metadata().Capabilities.TargetCompaction {
		return sessionUnsupported(s.ID(), SessionOperationCompactTarget)
	}
	return s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("compact/"+url.PathEscape(sessionID)), struct {
		AdditionalPrompt string `json:"additional_prompt,omitempty"`
	}{additionalPrompt}, nil)
}

func (s *remoteSession) AvailableModels(ctx context.Context) []ModelChoice {
	if !s.Metadata().Capabilities.ModelSwitching {
		return nil
	}
	var out []ModelChoice
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("models"), nil, &out); err != nil {
		return nil
	}
	return out
}

func (s *remoteSession) SetModel(ctx context.Context, modelRef string) error {
	if !s.Metadata().Capabilities.ModelSwitching {
		return sessionUnsupported(s.ID(), SessionOperationSetModel)
	}
	var metadata remoteSessionMetadata
	if err := s.runtime.client.sessionJSON(ctx, http.MethodPatch, s.endpoint("model"), struct {
		Model string `json:"model"`
	}{modelRef}, &metadata); err != nil {
		return err
	}
	s.mu.Lock()
	s.metadata = metadata.runtime()
	s.mu.Unlock()
	return s.refreshThinkingMetadata(ctx)
}

func (s *remoteSession) ThinkingLevels(ctx context.Context) []effort.Level {
	if !s.Metadata().Capabilities.ThinkingLevels {
		return nil
	}
	if err := s.refreshThinkingMetadata(ctx); err != nil {
		return nil
	}
	return slices.Clone(s.Metadata().ThinkingLevels)
}

func (s *remoteSession) CurrentThinkingLevel(ctx context.Context) effort.Level {
	if !s.Metadata().Capabilities.ThinkingLevels {
		return ""
	}
	if err := s.refreshThinkingMetadata(ctx); err != nil {
		return ""
	}
	return s.Metadata().ThinkingLevel
}

func (s *remoteSession) CycleThinkingLevel(ctx context.Context) (effort.Level, error) {
	if !s.Metadata().Capabilities.ThinkingLevels {
		return "", sessionUnsupported(s.ID(), SessionOperationThinkingLevel)
	}
	return s.mutateThinkingLevel(ctx, http.MethodPost, "thinking-level/cycle", nil)
}

func (s *remoteSession) SetThinkingLevel(ctx context.Context, level effort.Level) (effort.Level, error) {
	if !s.Metadata().Capabilities.ThinkingLevels {
		return "", sessionUnsupported(s.ID(), SessionOperationThinkingLevel)
	}
	return s.mutateThinkingLevel(ctx, http.MethodPatch, "thinking-level", struct {
		Level effort.Level `json:"level"`
	}{level})
}

func (s *remoteSession) refreshThinkingMetadata(ctx context.Context) error {
	if !s.Metadata().Capabilities.ThinkingLevels {
		return nil
	}
	_, err := s.mutateThinkingLevel(ctx, http.MethodGet, "thinking-level", nil)
	return err
}

func (s *remoteSession) mutateThinkingLevel(ctx context.Context, method, endpoint string, request any) (effort.Level, error) {
	var out api.SessionThinkingLevel
	if err := s.runtime.client.sessionJSON(ctx, method, s.endpoint(endpoint), request, &out); err != nil {
		return "", err
	}
	metadata := remoteSessionMetadata(out.Metadata).runtime()
	metadata.ThinkingLevels, metadata.ThinkingLevel = slices.Clone(out.Levels), out.Current
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadata = metadata
	return out.Current, nil
}

func (s *remoteSession) Skills(ctx context.Context) ([]skills.Skill, error) {
	if !s.Metadata().Capabilities.ForkSkills {
		return nil, nil // read-only query: no skills toolset means no skills
	}
	var out []skills.Skill
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("skills"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *remoteSession) ResolveSkillCommand(ctx context.Context, input string) (string, error) {
	if !s.Metadata().Capabilities.ForkSkills {
		return "", nil // read-only query: plain input is never an unsupported error
	}
	if !strings.HasPrefix(input, "/") {
		return "", nil
	}
	name, arg, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	availableSkills, err := s.Skills(ctx)
	if err != nil {
		return "", err
	}
	for _, skill := range availableSkills {
		if skill.Name == name && skill.IsFork() {
			return "", nil
		}
	}
	// Inline skill content is not exposed by the transport. Never claim
	// resolution for it; fork-mode discovery/execution remains supported.
	_ = arg
	return "", nil
}

func (s *remoteSession) StartSkillFork(ctx context.Context, operationID string, args skillstool.RunSkillArgs) error {
	if !s.Metadata().Capabilities.ForkSkills {
		return sessionUnsupported(s.ID(), SessionOperationRunSkill)
	}
	var accepted struct {
		SessionID   string `json:"session_id"`
		OperationID string `json:"operation_id"`
	}
	request := struct {
		skillstool.RunSkillArgs

		OperationID string `json:"operation_id"`
	}{RunSkillArgs: args, OperationID: operationID}
	if err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("skills/run"), request, &accepted); err != nil {
		return err
	}
	if accepted.SessionID != s.ID() || accepted.OperationID != operationID {
		return errors.New("invalid fork-skill operation identity")
	}
	return nil
}

func (s *remoteSession) RunSkillFork(ctx context.Context, args skillstool.RunSkillArgs, _ EventSink) (*tools.ToolCallResult, error) {
	if !s.Metadata().Capabilities.ForkSkills {
		return nil, sessionUnsupported(s.ID(), SessionOperationRunSkill)
	}
	var accepted struct {
		SessionID   string `json:"session_id"`
		OperationID string `json:"operation_id"`
	}
	if err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("skills/run"), args, &accepted); err != nil {
		return nil, err
	}
	if accepted.SessionID != s.ID() || accepted.OperationID == "" {
		return nil, errors.New("invalid fork-skill operation identity")
	}
	return tools.ResultSuccess(accepted.OperationID), nil
}

func (s *remoteSession) RefreshModelsCatalog(ctx context.Context) error {
	if !s.Metadata().Capabilities.ModelCatalogRefresh {
		return sessionUnsupported(s.ID(), SessionOperationRefreshModels)
	}
	var metadata remoteSessionMetadata
	if err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("models/refresh"), nil, &metadata); err != nil {
		return err
	}
	s.mu.Lock()
	s.metadata = metadata.runtime()
	s.mu.Unlock()
	return s.refreshThinkingMetadata(ctx)
}

func (s *remoteSession) TogglePause(ctx context.Context) (bool, error) {
	if !s.Metadata().Capabilities.Pause {
		return false, sessionUnsupported(s.ID(), SessionOperationPause)
	}
	var out struct {
		Paused bool `json:"paused"`
	}
	if err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("pause"), nil, &out); err != nil {
		return false, err
	}
	return out.Paused, nil
}

func (s *remoteSession) ContextBreakdown(ctx context.Context) (*ContextBreakdown, error) {
	if !s.Metadata().Capabilities.ContextInspection {
		return nil, sessionUnsupported(s.ID(), SessionOperationContext)
	}
	var out ContextBreakdown
	return &out, s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("context"), nil, &out)
}

func (s *remoteSession) LiveSessions(ctx context.Context) ([]LiveSession, error) {
	if !s.Metadata().Capabilities.LiveSessions {
		return nil, sessionUnsupported(s.ID(), SessionOperationLiveSessions)
	}
	var out []LiveSession
	err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("live-sessions"), nil, &out)
	return out, err
}

func (s *remoteSession) SetStarred(ctx context.Context, starred bool) error {
	if !s.Metadata().Capabilities.SessionEditing {
		return sessionUnsupported(s.ID(), SessionOperationSetStarred)
	}
	return s.runtime.client.sessionJSON(ctx, http.MethodPatch, s.endpoint("starred"), struct {
		Starred bool `json:"starred"`
	}{starred}, nil)
}

func (s *remoteSession) RemoveAttachment(ctx context.Context, attachmentPath string) error {
	if !s.Metadata().Capabilities.SessionEditing {
		return sessionUnsupported(s.ID(), SessionOperationRemoveAttachment)
	}
	return s.runtime.client.sessionJSON(ctx, http.MethodDelete, s.endpoint("attachments"), struct {
		Path string `json:"path"`
	}{attachmentPath}, nil)
}

func (s *remoteSession) SetTodoStatus(ctx context.Context, id, status string) ([]session.Todo, error) {
	if !s.Metadata().Capabilities.TodoEditing {
		return nil, sessionUnsupported(s.ID(), SessionOperationSetTodoStatus)
	}
	return s.editTodo(ctx, id, api.SessionTodoPatch{Status: &status})
}

func (s *remoteSession) RemoveTodo(ctx context.Context, id string) ([]session.Todo, error) {
	if !s.Metadata().Capabilities.TodoEditing {
		return nil, sessionUnsupported(s.ID(), SessionOperationRemoveTodo)
	}
	var out []session.Todo
	err := s.runtime.client.sessionJSON(ctx, http.MethodDelete, s.endpoint("todos/"+url.PathEscape(id)), nil, &out)
	return out, err
}

func (s *remoteSession) Todos(ctx context.Context) ([]session.Todo, error) {
	if !s.Metadata().Capabilities.Todos {
		return nil, sessionUnsupported(s.ID(), SessionOperationTodos)
	}
	var out []session.Todo
	if err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("todos"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *remoteSession) endpoint(operation string) string {
	return api.SessionAPIPath + "/" + url.PathEscape(s.ID()) + "/" + operation
}

type remoteSessionMetadata api.SessionMetadata

func (m remoteSessionMetadata) runtime() SessionMetadata {
	capabilities := m.Capabilities
	return SessionMetadata{SessionID: m.SessionID, AgentName: m.AgentName, Model: m.Model, ThinkingLevels: slices.Clone(m.ThinkingLevels), ThinkingLevel: m.ThinkingLevel, Capabilities: SessionCapabilities{
		ToolInspection: capabilities.ToolInspection, ToolsetRestart: capabilities.ToolsetRestart, PermissionsInspection: capabilities.PermissionsInspection, MCPPrompts: capabilities.MCPPrompts, TodoEditing: capabilities.TodoEditing, Branching: capabilities.Branching,
		AvailableModels: slices.Clone(capabilities.AvailableModels), Durability: subagent.Durability(capabilities.Durability),
		Compaction: capabilities.Compaction, TargetCompaction: capabilities.TargetCompaction, ModelSwitching: capabilities.ModelSwitching,
		DelegationPolicy: capabilities.DelegationPolicy, StopSubtree: capabilities.StopSubtree, ContextInspection: capabilities.ContextInspection, LiveSessions: capabilities.LiveSessions, SessionEditing: capabilities.SessionEditing,
		ForkSkills: capabilities.ForkSkills, Pause: capabilities.Pause, ModelCatalogRefresh: capabilities.ModelCatalogRefresh, ThinkingLevels: capabilities.ThinkingLevels,
		Todos: capabilities.Todos,
	}}
}

type (
	remoteSessionSnapshot      = api.SessionSnapshot[SessionState, InteractionKind, json.RawMessage]
	remoteSessionEnvelope      = api.SessionEnvelope[json.RawMessage]
	remoteSessionStreamMessage = api.SessionStreamMessage[SessionState, InteractionKind, json.RawMessage]
)

func (c *Client) sessionJSON(ctx context.Context, method, endpoint string, body, result any) error {
	// Even an injected client with no global timeout retains bounded CRUD.
	timeout := c.httpClient.Timeout
	if timeout <= 0 {
		timeout = defaultSessionRequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.sessionJSONWithClient(ctx, c.httpClient, method, endpoint, body, result)
}

func (c *Client) sessionJSONWithClient(ctx context.Context, client *http.Client, method, endpoint string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	u := *c.baseURL
	endpointPath, query, hasQuery := strings.Cut(endpoint, "?")
	u.Path = path.Join(u.Path, endpointPath)
	if hasQuery {
		u.RawQuery = query
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("session request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeSessionHTTPError(resp)
	}
	if result == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	// Responses are versioned by required identities; tolerate additive fields
	// so newer servers remain usable by older clients.
	if err := dec.Decode(result); err != nil {
		return fmt.Errorf("decode session response: %w", err)
	}
	return nil
}

func decodeSessionHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload struct {
		Message   any                `json:"message"`
		Error     string             `json:"error"`
		Operation string             `json:"operation"`
		Reason    SessionErrorReason `json:"reason"`
		SessionID string             `json:"session_id"`
		Detail    string             `json:"detail,omitempty"`
	}
	_ = json.Unmarshal(body, &payload)
	kind := SessionErrorInvalid
	validKind := func(candidate SessionErrorKind) bool {
		switch candidate {
		case SessionErrorPersistence, SessionErrorConflict, SessionErrorInterrupted, SessionErrorInvalid, SessionErrorNotFound, SessionErrorCapacity, SessionErrorStopped, SessionErrorClosed, SessionErrorStale, SessionErrorUnsupported, SessionErrorWrongSession:
			return true
		default:
			return false
		}
	}
	if candidate := SessionErrorKind(payload.Error); validKind(candidate) {
		kind = candidate
	} else {
		switch resp.StatusCode {
		case http.StatusServiceUnavailable:
			kind = SessionErrorPersistence
		case http.StatusNotFound:
			kind = SessionErrorNotFound
		case http.StatusTooManyRequests:
			kind = SessionErrorCapacity
		case http.StatusPreconditionFailed:
			kind = SessionErrorStale
		case http.StatusNotImplemented:
			kind = SessionErrorUnsupported
		case http.StatusConflict:
			kind = SessionErrorStopped
		}
	}
	return &sessionHTTPError{status: resp.StatusCode, err: &SessionError{Kind: kind, SessionID: payload.SessionID, Operation: SessionOperation(payload.Operation), Reason: payload.Reason, Detail: payload.Detail}}
}

func (c *Client) attachSession(ctx context.Context, id string, options ObserveOptions) (Observation, error) {
	u := *c.baseURL
	u.Path = path.Join(u.Path, api.SessionAPIPath+"/"+url.PathEscape(id)+"/events")
	if options.Tree {
		q := u.Query()
		q.Set("tree", "true")
		u.RawQuery = q.Encode()
	}
	if options.Since != nil {
		q := u.Query()
		q.Set("since", strconv.FormatUint(*options.Since, 10))
		if options.SinceEpoch != "" {
			q.Set("since_epoch", options.SinceEpoch)
		}
		u.RawQuery = q.Encode()
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	watchdog := newSessionStreamWatchdog(cancelStream, defaultSessionRequestTimeout, sessionStreamIdleTimeout)
	cancel := func() { watchdog.stop(); cancelStream() }
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if options.Since != nil {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(*options.Since, 10))
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.longLivedHTTPClient().Do(req) //nolint:bodyclose // ownership transfers to the observation goroutine on success
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		err := decodeSessionHTTPError(resp)
		cancel()
		return Observation{}, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return Observation{}, protocolError(fmt.Errorf("unexpected session stream HTTP status %d", resp.StatusCode))
	}
	scanner := bufio.NewScanner(sessionStreamReader{Reader: resp.Body, watchdog: watchdog})
	scanner.Split(scanSessionLines)
	scanner.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	first, err := scanSessionMessage(scanner)
	if err != nil {
		resp.Body.Close()
		cancel()
		return Observation{}, sessionStreamReadError(err)
	}
	if first.Version != sessionWireVersion || first.Type != "snapshot" || first.Snapshot == nil {
		resp.Body.Close()
		cancel()
		return Observation{}, protocolError(errors.New("session stream did not begin with a versioned snapshot"))
	}
	snapshot, err := c.decodeSessionSnapshot(*first.Snapshot)
	if err != nil {
		resp.Body.Close()
		cancel()
		return Observation{}, protocolError(err)
	}
	snapshots := []SessionSnapshot{snapshot}
	var replay []SessionEvent
	replaySequences := map[string]uint64{}
	for _, item := range snapshots {
		replaySequences[item.Status.SessionID] = item.Cursor
	}
	baseline := options.Since == nil || options.SinceEpoch != snapshot.Epoch
	replayEpochs := map[string]string{id: snapshot.Epoch}
	if !baseline {
		replaySequences[id] = *options.Since
	}
	for {
		message, scanErr := scanSessionMessage(scanner)
		if scanErr != nil {
			resp.Body.Close()
			cancel()
			return Observation{}, sessionStreamReadError(scanErr)
		}
		if message.Version != sessionWireVersion {
			resp.Body.Close()
			cancel()
			return Observation{}, protocolError(fmt.Errorf("unsupported session wire version %d", message.Version))
		}
		if options.Tree && message.Type == "snapshot" && message.Snapshot != nil {
			additional, decodeErr := c.decodeSessionSnapshot(*message.Snapshot)
			if decodeErr != nil {
				resp.Body.Close()
				cancel()
				return Observation{}, protocolError(decodeErr)
			}
			snapshots = append(snapshots, additional)
			replaySequences[additional.Status.SessionID] = additional.Cursor
			replayEpochs[additional.Status.SessionID] = additional.Epoch
			continue
		}
		if message.Type == "ready" {
			if !options.Tree && message.Cursor != snapshot.Cursor {
				resp.Body.Close()
				cancel()
				return Observation{}, protocolError(errors.New("session ready cursor does not match snapshot"))
			}
			break
		}
		if message.Type != "event" || message.Envelope == nil {
			resp.Body.Close()
			cancel()
			return Observation{}, protocolError(fmt.Errorf("unexpected session stream message %q", message.Type))
		}
		envelope, decodeErr := c.decodeSessionEnvelope(*message.Envelope)
		if decodeErr != nil {
			resp.Body.Close()
			cancel()
			return Observation{}, protocolError(decodeErr)
		}
		previous, known := replaySequences[envelope.SessionID]
		zeroSeed := baseline && known && envelope.IsLiveSeed()
		if envelope.Epoch != replayEpochs[envelope.SessionID] || (!options.Tree && envelope.SessionID != id) || (!envelope.Gap && !zeroSeed && envelope.Sequence == 0) || (envelope.Sequence != 0 && known && envelope.Sequence <= previous) {
			resp.Body.Close()
			cancel()
			return Observation{}, protocolError(errors.New("invalid session replay sequence or session"))
		}
		if envelope.Sequence != 0 {
			replaySequences[envelope.SessionID] = envelope.Sequence
		}
		replay = append(replay, envelope)
	}
	watchdog.touch(true)
	buffer := options.Buffer
	if buffer <= 0 {
		buffer = defaultEventChannelCapacity
	}
	var events chan SessionEvent
	if !options.Tree || !options.OrderedTree {
		events = make(chan SessionEvent, buffer)
	}
	var sessionsAdded chan SessionSnapshot
	var updates chan TreeUpdate
	if options.Tree {
		if options.OrderedTree {
			updates = make(chan TreeUpdate, buffer)
		} else {
			sessionsAdded = make(chan SessionSnapshot, buffer)
		}
	}
	errorsCh := make(chan error, 1)
	go func() {
		if events != nil {
			defer close(events)
		}
		if sessionsAdded != nil {
			defer close(sessionsAdded)
		}
		if updates != nil {
			defer close(updates)
		}
		defer close(errorsCh)
		defer resp.Body.Close()
		defer cancel()
		lastSequences := make(map[string]uint64, len(snapshots))
		for _, item := range snapshots {
			lastSequences[item.Status.SessionID] = item.Cursor
		}
		lastSequence := snapshot.Cursor
		for _, envelope := range replay {
			if envelope.Sequence != 0 {
				lastSequence = max(lastSequence, envelope.Sequence)
			}
		}
		for {
			m, e := scanSessionMessage(scanner)
			if e != nil {
				if watchdog.timedOut() {
					errorsCh <- fmt.Errorf("session observation idle timeout: %w", context.DeadlineExceeded)
				} else if streamCtx.Err() == nil {
					errorsCh <- fmt.Errorf("session observation terminated at cursor %d: %w", lastSequence, sessionStreamReadError(e))
				}
				return
			}
			if options.Tree && m.Version == sessionWireVersion && m.Type == "snapshot" && m.Snapshot != nil {
				additional, decodeErr := c.decodeSessionSnapshot(*m.Snapshot)
				if decodeErr != nil {
					errorsCh <- protocolError(decodeErr)
					return
				}
				if _, exists := lastSequences[additional.Status.SessionID]; exists {
					errorsCh <- protocolError(errors.New("duplicate tree session snapshot"))
					return
				}
				var seeds []SessionEvent
				for _, raw := range m.Snapshot.LiveSeeds {
					seed, err := c.decodeSessionEnvelope(raw)
					if err != nil || !seed.IsLiveSeed() || seed.SessionID != additional.Status.SessionID || seed.Epoch != additional.Epoch {
						errorsCh <- protocolError(errors.New("invalid dynamic tree live seed"))
						return
					}
					seeds = append(seeds, seed)
				}
				lastSequences[additional.Status.SessionID] = additional.Cursor
				replayEpochs[additional.Status.SessionID] = additional.Epoch
				if updates != nil {
					select {
					case updates <- TreeUpdate{Snapshot: &additional, Replay: seeds}:
					case <-streamCtx.Done():
						return
					}
				} else {
					select {
					case sessionsAdded <- additional:
					case <-streamCtx.Done():
						return
					}
					for _, seed := range seeds {
						select {
						case events <- seed:
						case <-streamCtx.Done():
							return
						}
					}
				}
				continue
			}
			if m.Version != sessionWireVersion || m.Type != "event" || m.Envelope == nil {
				errorsCh <- protocolError(fmt.Errorf("invalid session observation message at cursor %d", lastSequence))
				return
			}
			env, e := c.decodeSessionEnvelope(*m.Envelope)
			if e != nil {
				errorsCh <- protocolError(fmt.Errorf("decode session observation at cursor %d: %w", lastSequence, e))
				return
			}
			previous, known := lastSequences[env.SessionID]
			if !known || env.Epoch != replayEpochs[env.SessionID] || (!options.Tree && env.SessionID != id) || (!env.Gap && env.Sequence == 0) || (env.Sequence != 0 && env.Sequence <= previous) {
				errorsCh <- protocolError(fmt.Errorf("invalid session observation sequence %d after %d", env.Sequence, previous))
				return
			}
			if env.Sequence != 0 {
				lastSequences[env.SessionID], lastSequence = env.Sequence, env.Sequence
			}
			if updates != nil {
				select {
				case updates <- TreeUpdate{Event: &env}:
				case <-streamCtx.Done():
					return
				}
				continue
			}
			select {
			case events <- env:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return Observation{Initial: snapshots, TreeUpdates: updates, SessionsAdded: sessionsAdded, Replay: replay, Events: events, Errors: errorsCh, Cancel: func() { once.Do(cancel) }}, nil
}

const remoteSnapshotChunkBytes = 64 << 10

func scanSessionMessage(scanner *bufio.Scanner) (remoteSessionStreamMessage, error) {
	message, err := scanSessionFrame(scanner)
	if err != nil || message.Type != "snapshot_begin" {
		return message, err
	}
	if message.Version != sessionWireVersion {
		return message, errors.New("unsupported snapshot wire version")
	}
	reader := &snapshotChunkReader{scanner: scanner, cursor: message.Cursor}
	decoder := json.NewDecoder(reader)
	var snapshot remoteSessionSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return message, fmt.Errorf("decode chunked snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return message, fmt.Errorf("decode chunked snapshot ending: %w", err)
		}
		return message, errors.New("chunked snapshot has invalid ending")
	}
	if snapshot.Cursor != message.Cursor {
		return message, errors.New("chunked snapshot cursor mismatch")
	}
	return remoteSessionStreamMessage{Version: message.Version, Type: "snapshot", Snapshot: &snapshot}, nil
}

type snapshotChunkReader struct {
	scanner *bufio.Scanner
	cursor  uint64
	pending []byte
	done    bool
}

func (r *snapshotChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 && !r.done {
		message, err := scanSessionFrame(r.scanner)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if message.Version != sessionWireVersion || message.Cursor != r.cursor {
			return 0, errors.New("invalid snapshot chunk boundary")
		}
		switch message.Type {
		case "snapshot_chunk":
			if len(message.Chunk) == 0 || len(message.Chunk) > remoteSnapshotChunkBytes {
				return 0, errors.New("invalid snapshot chunk size")
			}
			r.pending = message.Chunk
		case "snapshot_end":
			r.done = true
		default:
			return 0, errors.New("unexpected snapshot chunk frame")
		}
	}
	if r.done {
		return 0, io.EOF
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func scanSessionFrame(scanner *bufio.Scanner) (remoteSessionStreamMessage, error) {
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			return remoteSessionStreamMessage{}, err
		}
		if head.Type == "snapshot_chunk" && len(data) > 2*remoteSnapshotChunkBytes {
			return remoteSessionStreamMessage{}, errors.New("snapshot chunk frame exceeds limit")
		}
		var m remoteSessionStreamMessage
		// SSE envelopes retain strict identity/version checks below while allowing
		// additive fields in envelopes and event payloads.
		if err := json.Unmarshal(data, &m); err != nil {
			return m, err
		}
		if m.Version != sessionWireVersion {
			return m, fmt.Errorf("unsupported session wire version %d (expected %d)", m.Version, sessionWireVersion)
		}
		return m, nil
	}
	if err := scanner.Err(); err != nil {
		return remoteSessionStreamMessage{}, err
	}
	return remoteSessionStreamMessage{}, io.EOF
}

func (c *Client) decodeSessionSnapshot(in remoteSessionSnapshot) (SessionSnapshot, error) {
	if in.Session == nil || in.Session.ID == "" || in.Status.SessionID != in.Session.ID {
		return SessionSnapshot{}, errors.New("invalid session snapshot identity")
	}
	out := SessionSnapshot{Session: in.Session, Status: SessionStatus(in.Status), Cursor: in.Cursor, Epoch: in.Epoch, TranscriptPosition: in.TranscriptPosition}
	for _, pending := range in.PendingInputs {
		out.PendingInputs = append(out.PendingInputs, PendingInput{TurnID: pending.TurnID, Content: pending.Content, MultiContent: pending.MultiContent, SessionPosition: pending.SessionPosition, InputOrigin: pending.InputOrigin, SenderID: pending.SenderID, SenderName: pending.SenderName, ReportOutcome: pending.ReportOutcome, InputMode: pending.InputMode})
	}
	for _, v := range in.Interactions {
		if v.SessionID != in.Session.ID {
			return out, errors.New("invalid session interaction identity")
		}
		e, err := c.decodeSessionEvent(v.Event)
		if err != nil {
			return out, err
		}
		out.Interactions = append(out.Interactions, InteractionSnapshot{SessionID: v.SessionID, InteractionID: v.InteractionID, ElicitationID: v.ElicitationID, Kind: v.Kind, Event: e})
	}
	return out, nil
}

func (c *Client) decodeSessionEnvelope(in remoteSessionEnvelope) (SessionEvent, error) {
	if in.Version != sessionWireVersion || in.SessionID == "" || (in.Sequence == 0 && !in.Gap && len(in.Event) == 0) {
		return SessionEvent{}, errors.New("invalid session envelope identity or version")
	}
	out := SessionEvent{Version: in.Version, SessionID: in.SessionID, TurnID: in.TurnID, InteractionID: in.InteractionID, Sequence: in.Sequence, Epoch: in.Epoch, TranscriptPosition: in.TranscriptPosition, Gap: in.Gap, FirstAvailable: in.FirstAvailable}
	if len(in.Event) > 0 {
		e, err := c.decodeSessionEvent(in.Event)
		if err != nil {
			return out, err
		}
		switch e.(type) {
		case *ToolCallConfirmationEvent, *MaxIterationsReachedEvent, *ElicitationRequestEvent, *InteractionResolvedEvent:
			if in.InteractionID == "" {
				return out, errors.New("interaction event is missing interaction_id")
			}
		}
		if resolved, ok := e.(*InteractionResolvedEvent); ok {
			if resolved.SessionID != in.SessionID || resolved.InteractionID != in.InteractionID {
				return out, errors.New("interaction resolution identity mismatch")
			}
			switch string(resolved.Reason) {
			case "responded", "canceled", "stopped":
			default:
				return out, errors.New("invalid interaction resolution reason")
			}
		}
		out.Event = e
	}
	return out, nil
}

func (c *Client) decodeSessionEvent(raw json.RawMessage) (Event, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	factory, ok := c.registry[head.Type]
	if !ok {
		return nil, fmt.Errorf("unknown session event type %q", head.Type)
	}
	event := factory()
	if err := json.Unmarshal(raw, event); err != nil {
		return nil, err
	}
	return event, nil
}

var (
	_ SessionRuntime = (*SessionTransport)(nil)
	_ SessionHandle  = (*remoteSession)(nil)
)

func (s *remoteSession) AwaitTurn(ctx context.Context, turnID string) error {
	return s.runtime.client.sessionWaitJSON(ctx, http.MethodPost, s.endpoint("turns/"+url.PathEscape(turnID)+"/wait"), nil, nil)
}

// PrepareSessionView holds only confirmed information on the client. The server
// owns no reservation across HTTP requests; Commit revalidates and publishes in
// one authenticated open_view request.
func (r *SessionTransport) PrepareSessionView(ctx context.Context, id string) (PreparedSessionView, error) {
	var response struct {
		Version int                     `json:"version"`
		View    string                  `json:"view"`
		Info    PreparedSessionViewInfo `json:"info"`
	}
	if err := r.client.sessionJSON(ctx, http.MethodGet, api.SessionAPIPath+"/"+url.PathEscape(id)+"?view=prepare-info", nil, &response); err != nil {
		return nil, err
	}
	if response.Version != sessionWireVersion || response.View != "prepare-info" {
		return nil, UnsupportedSessionOperation(id, "prepare_view")
	}
	if response.Info.SessionID != id || response.Info.Session == nil || response.Info.Session.ID != id || response.Info.Binding.AgentName == "" || response.Info.WorkingDir != response.Info.Session.WorkingDir {
		return nil, errors.New("invalid prepared session view identity")
	}
	child, cancel := context.WithCancel(ctx)
	return &remotePreparedView{runtime: r, info: cloneSessionViewInfo(response.Info), ctx: func() context.Context { return child }, cancel: cancel}, nil
}

type remotePreparedView struct {
	mu        sync.Mutex
	runtime   *SessionTransport
	info      PreparedSessionViewInfo
	ctx       func() context.Context // immutable confirmed preparation lifetime
	cancel    context.CancelFunc
	terminal  bool
	committed *CommittedSessionView
}

func (p *remotePreparedView) Info() PreparedSessionViewInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneSessionViewInfo(p.info)
}

func (p *remotePreparedView) Abort() {
	// Client preparation owns no server reservations. Cancellation is enough;
	// Commit checks this lifetime and owns any in-flight request cleanup.
	p.cancel()
}

func (p *remotePreparedView) Commit(ctx context.Context) (CommittedSessionView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed != nil {
		return CommittedSessionView{SessionHandle: p.committed.SessionHandle, Info: cloneSessionViewInfo(p.committed.Info)}, nil
	}
	if p.terminal || p.ctx().Err() != nil {
		return CommittedSessionView{}, context.Canceled
	}
	p.terminal = true
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx(), cancel)
	defer stop()
	defer cancel()
	defer p.cancel()
	var snapshot session.Session
	if err := p.runtime.client.sessionJSON(requestCtx, http.MethodPatch, api.SessionAPIPath+"/"+url.PathEscape(p.info.SessionID), SessionEdit{Kind: SessionEditOpenView}, &snapshot); err != nil {
		return CommittedSessionView{}, err
	}
	if snapshot.ID != p.info.SessionID || snapshot.ParentID != p.info.Session.ParentID {
		return CommittedSessionView{}, errors.New("committed session view identity changed")
	}
	handle, err := p.runtime.SessionByID(snapshot.ID)
	if err != nil {
		return CommittedSessionView{}, err
	}
	if hydrator, ok := handle.(Hydrator); ok {
		if err := hydrator.Hydrate(requestCtx); err != nil {
			return CommittedSessionView{}, err
		}
	}
	metadata := handle.Metadata()
	info := cloneSessionViewInfo(p.info)
	info.Session = &snapshot
	info.Binding.AgentName, info.Binding.Model = metadata.AgentName, metadata.Model
	info.WorkingDir = snapshot.WorkingDir
	if info.Binding.AgentName == "" {
		return CommittedSessionView{}, errors.New("committed session view binding is missing")
	}
	if info.Attach != nil {
		info.Attach.Session = snapshot.Clone()
		info.Attach.Agent = metadata.AgentName
	}
	result := CommittedSessionView{SessionHandle: handle, Info: info}
	p.committed = &result
	return CommittedSessionView{SessionHandle: handle, Info: cloneSessionViewInfo(info)}, nil
}

func (s *remoteSession) SetTodoDescription(ctx context.Context, id, expectedDescription, description string) ([]session.Todo, error) {
	if !s.Metadata().Capabilities.TodoEditing {
		return nil, sessionUnsupported(s.ID(), SessionOperationSetTodoDescription)
	}
	return s.editTodo(ctx, id, api.SessionTodoPatch{Description: &description, ExpectedDescription: &expectedDescription})
}
