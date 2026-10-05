// Package api defines transport-only schemas shared by servers and clients.
package api

import (
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
)

const (
	SessionAPIVersion          = 2
	SessionAPIPath             = "/api/v2/sessions"
	SessionCatalogDefaultLimit = 50
	SessionCatalogMaxLimit     = 200
)

type SessionCatalog[State ~string] struct {
	Version    int                      `json:"version"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Sources    []SessionCatalogSource   `json:"sources"`
	Sessions   []SessionResource[State] `json:"sessions"`
}

type SessionCatalogSource struct {
	Name      string `json:"name"`
	CanCreate bool   `json:"can_create"`
}

type SessionResource[State ~string] struct {
	SessionID            string                     `json:"session_id"`
	ParentID             string                     `json:"parent_id,omitempty"`
	Source               string                     `json:"source,omitempty"`
	AgentName            string                     `json:"agent_name,omitempty"`
	Title                string                     `json:"title,omitempty"`
	NumMessages          int                        `json:"num_messages"`
	Cost                 float64                    `json:"cost"`
	RequiresConfirmation bool                       `json:"requires_confirmation,omitempty"`
	CreatedAt            string                     `json:"created_at"`
	Messages             []session.Message          `json:"messages,omitempty"`
	ToolsApproved        bool                       `json:"tools_approved"`
	SafetyPolicy         session.SafetyPolicy       `json:"safety_policy,omitempty"`
	InputTokens          int64                      `json:"input_tokens"`
	OutputTokens         int64                      `json:"output_tokens"`
	WorkingDir           string                     `json:"working_dir,omitempty"`
	Permissions          *session.PermissionsConfig `json:"permissions,omitempty"`
	Starred              bool                       `json:"starred"`
	StateKnown           bool                       `json:"state_known"`
	State                State                      `json:"state,omitempty"`
	Activity             string                     `json:"activity,omitempty"`
	UpdatedAt            string                     `json:"updated_at"`
	Pending              *int                       `json:"pending,omitempty"`
	Interactions         *int                       `json:"interactions,omitempty"`
	LastError            string                     `json:"last_error,omitempty"`
	Loaded               bool                       `json:"loaded"`
	Attachable           bool                       `json:"attachable"`
	Loadable             bool                       `json:"loadable"`
	RouteError           string                     `json:"route_error,omitempty"`
}

type SessionCreateRequest struct {
	SessionID       string `json:"session_id,omitempty"`
	Source          string `json:"source,omitempty"`
	AgentName       string `json:"agent_name"`
	Model           string `json:"model,omitempty"`
	Title           string `json:"title,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// WorkingDir roots the session's toolsets in another directory. It is
	// validated against the configured --session-workingdir-root.
	WorkingDir string `json:"working_dir,omitempty"`
	// SafetyPolicy and ToolsApproved carry the client's safety choice. When
	// neither is set the agent's author-declared default applies.
	SafetyPolicy  session.SafetyPolicy       `json:"safety_policy,omitempty"`
	ToolsApproved bool                       `json:"tools_approved,omitempty"`
	Permissions   *session.PermissionsConfig `json:"permissions,omitempty"`
}

type SessionMetadata struct {
	SessionID      string              `json:"session_id"`
	AgentName      string              `json:"agent_name"`
	Model          string              `json:"model,omitempty"`
	ThinkingLevels []effort.Level      `json:"thinking_levels,omitempty"`
	ThinkingLevel  effort.Level        `json:"thinking_level,omitempty"`
	Capabilities   SessionCapabilities `json:"capabilities"`
}

type SessionCapabilities struct {
	Snapshots             bool     `json:"snapshots,omitempty"`
	DelegationPolicy      bool     `json:"delegation_policy,omitempty"`
	StopSubtree           bool     `json:"stop_subtree"`
	ToolInspection        bool     `json:"tool_inspection,omitempty"`
	ToolsetRestart        bool     `json:"toolset_restart,omitempty"`
	PermissionsInspection bool     `json:"permissions_inspection,omitempty"`
	MCPPrompts            bool     `json:"mcp_prompts,omitempty"`
	TodoEditing           bool     `json:"todo_editing,omitempty"`
	Branching             bool     `json:"branching,omitempty"`
	AvailableModels       []string `json:"available_models,omitempty"`
	Durability            string   `json:"durability,omitempty"`
	Compaction            bool     `json:"compaction,omitempty"`
	TargetCompaction      bool     `json:"target_compaction,omitempty"`
	ModelSwitching        bool     `json:"model_switching,omitempty"`
	ContextInspection     bool     `json:"context_inspection,omitempty"`
	LiveSessions          bool     `json:"live_sessions,omitempty"`
	SessionEditing        bool     `json:"session_editing,omitempty"`
	ForkSkills            bool     `json:"fork_skills,omitempty"`
	Pause                 bool     `json:"pause,omitempty"`
	ModelCatalogRefresh   bool     `json:"model_catalog_refresh,omitempty"`
	ThinkingLevels        bool     `json:"thinking_levels,omitempty"`
	Todos                 bool     `json:"todos,omitempty"`
}

type SessionThinkingLevel struct {
	Levels   []effort.Level  `json:"levels"`
	Current  effort.Level    `json:"current,omitempty"`
	Metadata SessionMetadata `json:"metadata"`
}

type SessionInputRequest struct {
	GenerateTitle bool               `json:"generate_title,omitempty"`
	Mode          string             `json:"mode,omitempty"`
	Content       string             `json:"content"`
	MultiContent  []chat.MessagePart `json:"multi_content,omitempty"`
	RequestID     string             `json:"request_id,omitempty"`
}

type SessionSubmission[Disposition ~string] struct {
	SessionID   string      `json:"session_id"`
	TurnID      string      `json:"turn_id"`
	Disposition Disposition `json:"disposition,omitempty"`
}

type SessionCancelRequest struct {
	TurnID string `json:"turn_id,omitempty"`
}

type SessionResponseRequest[Kind ~string] struct {
	InteractionID string         `json:"interaction_id"`
	Kind          Kind           `json:"kind"`
	Confirmation  string         `json:"confirmation,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	ToolName      string         `json:"tool_name,omitempty"`
	ElicitationID string         `json:"elicitation_id,omitempty"`
	Action        string         `json:"action,omitempty"`
	Content       map[string]any `json:"content,omitempty"`
	ClientID      string         `json:"client_id,omitempty"`
}

type SessionStatus[State ~string] struct {
	InterruptedTurns int    `json:"interrupted_turns,omitempty"`
	SessionID        string `json:"session_id"`
	AgentName        string `json:"agent_name"`
	State            State  `json:"state"`
	Pending          int    `json:"pending"`
	TurnID           string `json:"turn_id,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	Dormant          bool   `json:"dormant,omitempty"`
	PauseArmed       bool   `json:"pause_armed,omitempty"`
	Paused           bool   `json:"paused,omitempty"`
	PauseGeneration  uint64 `json:"pause_generation,omitempty"`
}

type SessionPendingInput struct {
	InputOrigin     session.InputOrigin   `json:"input_origin,omitempty"`
	SenderID        string                `json:"sender_id,omitempty"`
	SenderName      string                `json:"sender_name,omitempty"`
	ReportOutcome   session.ReportOutcome `json:"report_outcome,omitempty"`
	InputMode       string                `json:"input_mode,omitempty"`
	TurnID          string                `json:"turn_id"`
	Content         string                `json:"content"`
	MultiContent    []chat.MessagePart    `json:"multi_content,omitempty"`
	SessionPosition int                   `json:"session_position"`
}

type SessionInteraction[Kind ~string, Event any] struct {
	SessionID     string `json:"session_id"`
	InteractionID string `json:"interaction_id"`
	Kind          Kind   `json:"kind"`
	ElicitationID string `json:"elicitation_id,omitempty"`
	Event         Event  `json:"event"`
}

type SessionSnapshot[State ~string, Kind ~string, Event any] struct {
	ParentSessionID string                            `json:"parent_session_id,omitempty"`
	Session         *session.Session                  `json:"session"`
	Status          SessionStatus[State]              `json:"status"`
	Interactions    []SessionInteraction[Kind, Event] `json:"interactions"`
	PendingInputs   []SessionPendingInput             `json:"pending_inputs"`
	// LiveSeeds supplements a dynamically admitted tree snapshot before its live tail.
	LiveSeeds          []SessionEnvelope[Event] `json:"live_seeds,omitempty"`
	Epoch              string                   `json:"epoch,omitempty"`
	Cursor             uint64                   `json:"cursor"`
	TranscriptPosition int                      `json:"transcript_position"`
	Presentation       []Event                  `json:"presentation,omitempty"`
	TitleStatus        string                   `json:"title_status,omitempty"`
}

// SessionEnvelope carries ordered journal events. Before "ready", a stream
// opened without a cursor may also carry sequence-zero live seeds supplementing
// an already supplied session snapshot: stream_started, agent_choice_reasoning,
// agent_choice, partial_tool_call, tool_call, and tool_call_output. Seeds have
// matching envelope/event session identities where the event carries one,
// no turn/interaction identity, transcript_position -1, and no gap metadata.
// They do not advance the replay cursor and are never live-tail journal events.
// Dynamic tree admission carries the same seeds in snapshot.live_seeds.
type SessionEnvelope[Event any] struct {
	Version            int    `json:"version"`
	SessionID          string `json:"session_id"`
	TurnID             string `json:"turn_id,omitempty"`
	InteractionID      string `json:"interaction_id,omitempty"`
	Epoch              string `json:"epoch,omitempty"`
	Sequence           uint64 `json:"sequence"`
	TranscriptPosition int    `json:"transcript_position"`
	Event              Event  `json:"event,omitempty"`
	Gap                bool   `json:"gap,omitempty"`
	FirstAvailable     uint64 `json:"first_available,omitempty"`
}

type SessionStreamMessage[State ~string, Kind ~string, Event any] struct {
	Version  int                                  `json:"version"`
	Type     string                               `json:"type"`
	Snapshot *SessionSnapshot[State, Kind, Event] `json:"snapshot,omitempty"`
	Envelope *SessionEnvelope[Event]              `json:"envelope,omitempty"`
	Cursor   uint64                               `json:"cursor,omitempty"`
	Chunk    []byte                               `json:"chunk,omitempty"`
}

type SessionSummaryCatalog[Row any] struct {
	Version    int    `json:"version"`
	View       string `json:"view"`
	Sessions   []Row  `json:"sessions"`
	NextCursor string `json:"next_cursor,omitempty"`
}
