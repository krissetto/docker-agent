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

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

const sessionWireVersion = 1

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
	if sess == nil {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "create_session"}
	}
	request := struct {
		Source          string `json:"source,omitempty"`
		AgentName       string `json:"agent_name"`
		Model           string `json:"model,omitempty"`
		Title           string `json:"title,omitempty"`
		ParentSessionID string `json:"parent_session_id,omitempty"`
	}{r.source, binding.AgentName, binding.Model, sess.TitleSnapshot(), binding.ParentSessionID}
	var metadata remoteSessionMetadata
	if err := r.client.sessionJSON(ctx, http.MethodPost, "/api/sessions", request, &metadata); err != nil {
		return nil, err
	}
	if metadata.SessionID == "" {
		return nil, errors.New("session create response has empty session_id")
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
	if err := r.client.sessionJSON(ctx, http.MethodPost, "/api/sessions/"+url.PathEscape(sessionID)+"/switch-agent", struct {
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
	return r.client.sessionJSON(ctx, http.MethodDelete, "/api/sessions/"+url.PathEscape(id), nil, nil)
}

func (r *SessionTransport) ListSessions(ctx context.Context) ([]SessionCatalogEntry, error) {
	var catalog struct {
		Sessions []struct {
			SessionID  string            `json:"session_id"`
			Title      string            `json:"title"`
			AgentName  string            `json:"agent_name"`
			UpdatedAt  string            `json:"updated_at"`
			CreatedAt  time.Time         `json:"created_at"`
			Starred    bool              `json:"starred"`
			Loadable   bool              `json:"loadable"`
			Messages   []session.Message `json:"messages"`
			WorkingDir string            `json:"working_dir"`
		} `json:"sessions"`
	}
	if err := r.client.sessionJSON(ctx, http.MethodGet, "/api/sessions", nil, &catalog); err != nil {
		return nil, err
	}
	out := make([]SessionCatalogEntry, 0, len(catalog.Sessions))
	for _, row := range catalog.Sessions {
		out = append(out, SessionCatalogEntry{SessionID: row.SessionID, Title: row.Title, AgentName: row.AgentName, UpdatedAt: row.UpdatedAt, CreatedAt: row.CreatedAt, Starred: row.Starred, Loadable: row.Loadable, NumMessages: len(row.Messages), WorkingDir: row.WorkingDir})
	}
	return out, nil
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
	request := struct {
		Content      string             `json:"content"`
		MultiContent []chat.MessagePart `json:"multi_content,omitempty"`
		Mode         string             `json:"mode,omitempty"`
		ClientID     string             `json:"client_id,omitempty"`
	}{input.Content, input.MultiContent, operation, input.ClientID}
	var out Submission
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("messages"), request, &out)
	if err == nil && (out.SessionID != s.ID() || out.TurnID == "") {
		return Submission{}, fmt.Errorf("invalid session submission identity: session=%q turn=%q", out.SessionID, out.TurnID)
	}
	return out, err
}

func (s *remoteSession) Retry(ctx context.Context) (Submission, error) {
	var out Submission
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("retry"), struct{}{}, &out)
	if err == nil && (out.SessionID != s.ID() || out.TurnID == "") {
		return Submission{}, fmt.Errorf("invalid session retry identity: session=%q turn=%q", out.SessionID, out.TurnID)
	}
	return out, err
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
		Metadata remoteSessionMetadata `json:"metadata"`
		Status   SessionStatus         `json:"status"`
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
	return out.Status, nil
}

func (s *remoteSession) Respond(ctx context.Context, response InteractionResponse) error {
	request := struct {
		InteractionID string          `json:"interaction_id"`
		Kind          InteractionKind `json:"kind"`
		Confirmation  string          `json:"confirmation,omitempty"`
		Reason        string          `json:"reason,omitempty"`
		ToolName      string          `json:"tool_name,omitempty"`
		ElicitationID string          `json:"elicitation_id,omitempty"`
		Action        string          `json:"action,omitempty"`
		Content       map[string]any  `json:"content,omitempty"`
		ClientID      string          `json:"client_id,omitempty"`
	}{InteractionID: response.InteractionID, Kind: response.Kind, Confirmation: string(response.Resume.Type), Reason: response.Resume.Reason, ToolName: response.Resume.ToolName, ElicitationID: response.ElicitationID, Action: string(response.Elicitation.Action), Content: response.Elicitation.Content, ClientID: response.ClientID}
	return s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("responses"), request, nil)
}

func (s *remoteSession) UpdateTitle(ctx context.Context, title string) error {
	return s.runtime.client.sessionJSON(ctx, http.MethodPatch, s.endpoint("title"), struct {
		Title string `json:"title"`
	}{title}, nil)
}

func (s *remoteSession) Cancel(ctx context.Context, turnID string) (CancelResult, error) {
	var out CancelResult
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("cancel"), struct {
		TurnID string `json:"turn_id,omitempty"`
	}{turnID}, &out)
	if err == nil && out.SessionID != s.ID() {
		return CancelResult{}, fmt.Errorf("invalid session cancel identity %q", out.SessionID)
	}
	return out, err
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
	var out struct {
		Levels   []effort.Level        `json:"levels"`
		Current  effort.Level          `json:"current"`
		Metadata remoteSessionMetadata `json:"metadata"`
	}
	if err := s.runtime.client.sessionJSON(ctx, method, s.endpoint(endpoint), request, &out); err != nil {
		return "", err
	}
	metadata := out.Metadata.runtime()
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

func (s *remoteSession) EmitPinnedAgentInfo(context.Context, EventSink) {}

func (s *remoteSession) endpoint(operation string) string {
	return "/api/sessions/" + url.PathEscape(s.ID()) + "/" + operation
}

type remoteSessionMetadata struct {
	SessionID    string `json:"session_id"`
	AgentName    string `json:"agent_name"`
	Model        string `json:"model,omitempty"`
	Capabilities struct {
		AvailableModels     []string `json:"available_models,omitempty"`
		Durability          string   `json:"durability,omitempty"`
		Compaction          bool     `json:"compaction,omitempty"`
		TargetCompaction    bool     `json:"target_compaction,omitempty"`
		ModelSwitching      bool     `json:"model_switching,omitempty"`
		ContextInspection   bool     `json:"context_inspection,omitempty"`
		LiveSessions        bool     `json:"live_sessions,omitempty"`
		SessionEditing      bool     `json:"session_editing,omitempty"`
		ForkSkills          bool     `json:"fork_skills,omitempty"`
		Pause               bool     `json:"pause,omitempty"`
		ModelCatalogRefresh bool     `json:"model_catalog_refresh,omitempty"`
		ThinkingLevels      bool     `json:"thinking_levels,omitempty"`
		Todos               bool     `json:"todos,omitempty"`
	} `json:"capabilities"`
	ThinkingLevels []effort.Level `json:"thinking_levels,omitempty"`
	ThinkingLevel  effort.Level   `json:"thinking_level,omitempty"`
}

func (m remoteSessionMetadata) runtime() SessionMetadata {
	capabilities := m.Capabilities
	return SessionMetadata{SessionID: m.SessionID, AgentName: m.AgentName, Model: m.Model, ThinkingLevels: slices.Clone(m.ThinkingLevels), ThinkingLevel: m.ThinkingLevel, Capabilities: SessionCapabilities{
		AvailableModels: slices.Clone(capabilities.AvailableModels), Durability: subagent.Durability(capabilities.Durability),
		Compaction: capabilities.Compaction, TargetCompaction: capabilities.TargetCompaction, ModelSwitching: capabilities.ModelSwitching,
		ContextInspection: capabilities.ContextInspection, LiveSessions: capabilities.LiveSessions, SessionEditing: capabilities.SessionEditing,
		ForkSkills: capabilities.ForkSkills, Pause: capabilities.Pause, ModelCatalogRefresh: capabilities.ModelCatalogRefresh, ThinkingLevels: capabilities.ThinkingLevels,
		Todos: capabilities.Todos,
	}}
}

type remoteSessionSnapshot struct {
	Session      *session.Session `json:"session"`
	Status       SessionStatus    `json:"status"`
	Interactions []struct {
		SessionID     string          `json:"session_id"`
		InteractionID string          `json:"interaction_id"`
		ElicitationID string          `json:"elicitation_id"`
		Kind          InteractionKind `json:"kind"`
		Event         json.RawMessage `json:"event"`
	} `json:"interactions"`
	PendingInputs []struct {
		TurnID          string             `json:"turn_id"`
		Content         string             `json:"content"`
		MultiContent    []chat.MessagePart `json:"multi_content,omitempty"`
		SessionPosition int                `json:"session_position"`
	} `json:"pending_inputs"`
	Cursor             uint64 `json:"cursor"`
	TranscriptPosition int    `json:"transcript_position"`
}
type remoteSessionEnvelope struct {
	Version            int             `json:"version"`
	SessionID          string          `json:"session_id"`
	TurnID             string          `json:"turn_id,omitempty"`
	InteractionID      string          `json:"interaction_id,omitempty"`
	Sequence           uint64          `json:"sequence"`
	TranscriptPosition int             `json:"transcript_position"`
	Event              json.RawMessage `json:"event,omitempty"`
	Gap                bool            `json:"gap,omitempty"`
	FirstAvailable     uint64          `json:"first_available,omitempty"`
}
type remoteSessionStreamMessage struct {
	Version  int                    `json:"version"`
	Type     string                 `json:"type"`
	Snapshot *remoteSessionSnapshot `json:"snapshot,omitempty"`
	Envelope *remoteSessionEnvelope `json:"envelope,omitempty"`
	Cursor   uint64                 `json:"cursor,omitempty"`
}

func (c *Client) sessionJSON(ctx context.Context, method, endpoint string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	u := *c.baseURL
	u.Path = path.Join(u.Path, endpoint)
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
	resp, err := c.httpClient.Do(req)
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
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
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
	}
	_ = json.Unmarshal(body, &payload)
	kind := SessionErrorInvalid
	validKind := func(candidate SessionErrorKind) bool {
		switch candidate {
		case SessionErrorInvalid, SessionErrorNotFound, SessionErrorCapacity, SessionErrorStopped, SessionErrorClosed, SessionErrorStale, SessionErrorUnsupported, SessionErrorWrongSession:
			return true
		default:
			return false
		}
	}
	if candidate := SessionErrorKind(payload.Error); validKind(candidate) {
		kind = candidate
	} else {
		switch resp.StatusCode {
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
	return &SessionError{Kind: kind, SessionID: payload.SessionID, Operation: SessionOperation(payload.Operation), Reason: payload.Reason}
}

func (c *Client) attachSession(ctx context.Context, id string, options ObserveOptions) (Observation, error) {
	u := *c.baseURL
	u.Path = path.Join(u.Path, "/api/sessions/"+url.PathEscape(id)+"/events")
	if options.Tree {
		q := u.Query()
		q.Set("tree", "true")
		u.RawQuery = q.Encode()
	}
	if options.Since != nil {
		q := u.Query()
		q.Set("since", strconv.FormatUint(*options.Since, 10))
		u.RawQuery = q.Encode()
	}
	streamCtx, cancel := context.WithCancel(ctx)
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
	resp, err := c.httpClient.Do(req) //nolint:bodyclose // ownership transfers to the observation goroutine on success
	if err != nil {
		cancel()
		return Observation{}, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		cancel()
		return Observation{}, decodeSessionHTTPError(resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	first, err := scanSessionMessage(scanner)
	if err != nil {
		resp.Body.Close()
		cancel()
		return Observation{}, err
	}
	if first.Version != sessionWireVersion || first.Type != "snapshot" || first.Snapshot == nil {
		resp.Body.Close()
		cancel()
		return Observation{}, errors.New("session stream did not begin with a versioned snapshot")
	}
	snapshot, err := c.decodeSessionSnapshot(*first.Snapshot)
	if err != nil {
		resp.Body.Close()
		cancel()
		return Observation{}, err
	}
	snapshots := []SessionSnapshot{snapshot}
	var replay []SessionEvent
	replaySequences := map[string]uint64{}
	for _, item := range snapshots {
		replaySequences[item.Status.SessionID] = item.Cursor
	}
	if options.Since != nil {
		replaySequences[id] = *options.Since
	}
	for {
		message, scanErr := scanSessionMessage(scanner)
		if scanErr != nil {
			resp.Body.Close()
			cancel()
			return Observation{}, scanErr
		}
		if message.Version != sessionWireVersion {
			resp.Body.Close()
			cancel()
			return Observation{}, fmt.Errorf("unsupported session wire version %d", message.Version)
		}
		if options.Tree && message.Type == "snapshot" && message.Snapshot != nil {
			additional, decodeErr := c.decodeSessionSnapshot(*message.Snapshot)
			if decodeErr != nil {
				resp.Body.Close()
				cancel()
				return Observation{}, decodeErr
			}
			snapshots = append(snapshots, additional)
			continue
		}
		if message.Type == "ready" {
			if !options.Tree && message.Cursor != snapshot.Cursor {
				resp.Body.Close()
				cancel()
				return Observation{}, errors.New("session ready cursor does not match snapshot")
			}
			break
		}
		if message.Type != "event" || message.Envelope == nil {
			resp.Body.Close()
			cancel()
			return Observation{}, fmt.Errorf("unexpected session stream message %q", message.Type)
		}
		envelope, decodeErr := c.decodeSessionEnvelope(*message.Envelope)
		if decodeErr != nil {
			resp.Body.Close()
			cancel()
			return Observation{}, decodeErr
		}
		previous, known := replaySequences[envelope.SessionID]
		zeroSeed := options.Tree && envelope.Sequence == 0 && envelope.Event != nil
		if (!options.Tree && envelope.SessionID != id) || (!envelope.Gap && !zeroSeed && envelope.Sequence == 0) || (envelope.Sequence != 0 && known && envelope.Sequence <= previous) {
			resp.Body.Close()
			cancel()
			return Observation{}, errors.New("invalid session replay sequence or session")
		}
		if envelope.Sequence != 0 {
			replaySequences[envelope.SessionID] = envelope.Sequence
		}
		replay = append(replay, envelope)
	}
	buffer := options.Buffer
	if buffer <= 0 {
		buffer = defaultEventChannelCapacity
	}
	events := make(chan SessionEvent, buffer)
	var sessionsAdded chan SessionSnapshot
	if options.Tree {
		sessionsAdded = make(chan SessionSnapshot, buffer)
	}
	errorsCh := make(chan error, 1)
	go func() {
		defer close(events)
		if sessionsAdded != nil {
			defer close(sessionsAdded)
		}
		defer close(errorsCh)
		defer resp.Body.Close()
		defer cancel()
		lastSequences := make(map[string]uint64, len(snapshots))
		for _, item := range snapshots {
			lastSequences[item.Status.SessionID] = item.Cursor
		}
		lastSequence := snapshot.Cursor
		if len(replay) > 0 {
			lastSequence = replay[len(replay)-1].Sequence
		}
		for {
			m, e := scanSessionMessage(scanner)
			if e != nil {
				if streamCtx.Err() == nil {
					errorsCh <- fmt.Errorf("session observation terminated at cursor %d: %w", lastSequence, e)
				}
				return
			}
			if options.Tree && m.Type == "snapshot" && m.Snapshot != nil {
				additional, decodeErr := c.decodeSessionSnapshot(*m.Snapshot)
				if decodeErr != nil {
					errorsCh <- decodeErr
					return
				}
				lastSequences[additional.Status.SessionID] = additional.Cursor
				select {
				case sessionsAdded <- additional:
				case <-streamCtx.Done():
					return
				}
				continue
			}
			if m.Version != sessionWireVersion || m.Type != "event" || m.Envelope == nil {
				errorsCh <- fmt.Errorf("invalid session observation message at cursor %d", lastSequence)
				return
			}
			env, e := c.decodeSessionEnvelope(*m.Envelope)
			if e != nil {
				errorsCh <- fmt.Errorf("decode session observation at cursor %d: %w", lastSequence, e)
				return
			}
			previous := lastSequences[env.SessionID]
			if (!options.Tree && env.SessionID != id) || (env.Sequence != 0 && env.Sequence <= previous) {
				errorsCh <- fmt.Errorf("invalid session observation sequence %d after %d", env.Sequence, previous)
				return
			}
			if env.Sequence != 0 {
				lastSequences[env.SessionID], lastSequence = env.Sequence, env.Sequence
			}
			select {
			case events <- env:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return Observation{Initial: snapshots, SessionsAdded: sessionsAdded, Replay: replay, Events: events, Errors: errorsCh, Cancel: func() { once.Do(cancel) }}, nil
}

func scanSessionMessage(scanner *bufio.Scanner) (remoteSessionStreamMessage, error) {
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok {
			continue
		}
		var m remoteSessionStreamMessage
		// SSE envelopes retain strict identity/version checks below while allowing
		// additive fields in envelopes and event payloads.
		if err := json.Unmarshal(data, &m); err != nil {
			return m, err
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
	out := SessionSnapshot{Session: in.Session, Status: in.Status, Cursor: in.Cursor, TranscriptPosition: in.TranscriptPosition}
	for _, pending := range in.PendingInputs {
		out.PendingInputs = append(out.PendingInputs, PendingInput{TurnID: pending.TurnID, Content: pending.Content, MultiContent: pending.MultiContent, SessionPosition: pending.SessionPosition})
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
	out := SessionEvent{Version: in.Version, SessionID: in.SessionID, TurnID: in.TurnID, InteractionID: in.InteractionID, Sequence: in.Sequence, TranscriptPosition: in.TranscriptPosition, Gap: in.Gap, FirstAvailable: in.FirstAvailable}
	if len(in.Event) > 0 {
		e, err := c.decodeSessionEvent(in.Event)
		if err != nil {
			return out, err
		}
		switch e.(type) {
		case *ToolCallConfirmationEvent, *MaxIterationsReachedEvent, *ElicitationRequestEvent:
			if in.InteractionID == "" {
				return out, errors.New("interaction event is missing interaction_id")
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
