package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// SessionAgentAttribute persists the immutable session binding in stores whose
// legacy session schema does not have a dedicated agent column.
const SessionAgentAttribute = "docker-agent.actor.agent"

const SessionParentAgentAttribute = "docker-agent.actor.parent_agent"

// SessionErrorKind is a stable, machine-readable session operation failure.
type SessionErrorKind string

const (
	SessionErrorConflict    SessionErrorKind = "conflict"
	SessionErrorPersistence SessionErrorKind = "persistence"
)

// SessionOperation identifies a stable session operation.
type SessionOperation string

const (
	SessionOperationActiveDescendants     SessionOperation = "active_descendants"
	SessionOperationActiveDescendantsRoot SessionOperation = "active_descendants_root"
	SessionOperationAdmitChildRun         SessionOperation = "admit_child_run"
	SessionOperationAdmitChildRunning     SessionOperation = "admit_child_running"
	SessionOperationAttach                SessionOperation = "attach"
	SessionOperationBindAgent             SessionOperation = "bind_agent"
	SessionOperationCompact               SessionOperation = "compact"
	SessionOperationCompactBusy           SessionOperation = "compact_busy"
	SessionOperationCompactPending        SessionOperation = "compact_pending"
	SessionOperationCompactTarget         SessionOperation = "compact_target"
	SessionOperationContext               SessionOperation = "context"
	SessionOperationCreateParent          SessionOperation = "create_parent"
	SessionOperationCreateSession         SessionOperation = "create_session"
	SessionOperationDurableSession        SessionOperation = "durable_session"
	SessionOperationFork                  SessionOperation = "fork"
	SessionOperationGate                  SessionOperation = "gate"
	SessionOperationInspectTree           SessionOperation = "inspect_tree"
	SessionOperationLiveSessions          SessionOperation = "live_sessions"
	SessionOperationLookup                SessionOperation = "lookup"
	SessionOperationModels                SessionOperation = "models"
	SessionOperationObserve               SessionOperation = "observe"
	SessionOperationObserveTree           SessionOperation = "observe_tree"
	SessionOperationObserveTreeCursor     SessionOperation = "observe_tree_cursor"
	SessionOperationPause                 SessionOperation = "pause"
	SessionOperationPost                  SessionOperation = "post"
	SessionOperationPromoteInput          SessionOperation = "promote_input"
	SessionOperationRefreshModels         SessionOperation = "refresh_models"
	SessionOperationRegister              SessionOperation = "register"
	SessionOperationReleaseActive         SessionOperation = "release_active"
	SessionOperationRemoveAttachment      SessionOperation = "remove_attachment"
	SessionOperationReplacePinnedSession  SessionOperation = "replace_pinned_session"
	SessionOperationResolveSession        SessionOperation = "resolve_session"
	SessionOperationRespond               SessionOperation = "respond"
	SessionOperationRespondElicitation    SessionOperation = "respond_elicitation"
	SessionOperationRespondGeneration     SessionOperation = "respond_generation"
	SessionOperationRespondKind           SessionOperation = "respond_kind"
	SessionOperationRespondResume         SessionOperation = "respond_resume"
	SessionOperationRestoreActivate       SessionOperation = "restore_activate"
	SessionOperationRestoreAncestry       SessionOperation = "restore_ancestry"
	SessionOperationRestoreBinding        SessionOperation = "restore_binding"
	SessionOperationRestoreChildTree      SessionOperation = "restore_child_tree"
	SessionOperationRestoreCollision      SessionOperation = "restore_collision"
	SessionOperationRestoreParent         SessionOperation = "restore_parent"
	SessionOperationRestorePrepare        SessionOperation = "restore_prepare"
	SessionOperationRestoreReserved       SessionOperation = "restore_reserved"
	SessionOperationRestoreSource         SessionOperation = "restore_source"
	SessionOperationRestoreTree           SessionOperation = "restore_tree"
	SessionOperationRestoreTreeMembership SessionOperation = "restore_tree_membership"
	SessionOperationRunSkill              SessionOperation = "run_skill"
	SessionOperationSend                  SessionOperation = "send"
	SessionOperationSetModel              SessionOperation = "set_model"
	SessionOperationSetStarred            SessionOperation = "set_starred"
	SessionOperationSkillBusy             SessionOperation = "skill_busy"
	SessionOperationSkills                SessionOperation = "skills"
	SessionOperationSource                SessionOperation = "source"
	SessionOperationStart                 SessionOperation = "start"
	SessionOperationStartGate             SessionOperation = "start_gate"
	SessionOperationSteer                 SessionOperation = "steer"
	SessionOperationSubagentAdmission     SessionOperation = "subagent_admission"
	SessionOperationSubagentDepth         SessionOperation = "subagent_depth"
	SessionOperationSubmit                SessionOperation = "submit"
	SessionOperationSwitchAgent           SessionOperation = "switch_agent"
	SessionOperationSwitchAttachedAgent   SessionOperation = "switch_attached_agent"
	SessionOperationThinkingLevel         SessionOperation = "thinking_level"
	SessionOperationTodos                 SessionOperation = "todos"
	SessionOperationSetTodoStatus         SessionOperation = "set_todo_status"
	SessionOperationRemoveTodo            SessionOperation = "remove_todo"
	SessionOperationSetTodoDescription    SessionOperation = "set_todo_description"
	SessionOperationUpdateTitle           SessionOperation = "update_title"
	SessionOperationWakePending           SessionOperation = "wake_pending"
)

// SessionErrorReason identifies the capacity condition behind a session error.
type SessionErrorReason string

const (
	SessionErrorReasonBusy    SessionErrorReason = "busy"
	SessionErrorReasonPending SessionErrorReason = "pending"
	SessionErrorReasonLimit   SessionErrorReason = "limit"
)

// SessionRuntime is the borrowed session registry. It cannot shut down its owner.
type SessionRuntime interface {
	CreateSession(ctx context.Context, sess *session.Session, binding SessionBinding) (SessionHandle, error)
	SessionByID(sessionID string) (SessionHandle, error)
	DeleteSession(ctx context.Context, sessionID string) error
}

// Hydrator resolves transport metadata after an ID-only lookup without
// changing the historical SessionRuntime interface.
type Hydrator interface {
	Hydrate(ctx context.Context) error
}

// SessionCatalogEntry is the portable session browser row.
type SessionCatalogEntry struct {
	SessionID   string
	Title       string
	AgentName   string
	UpdatedAt   string
	CreatedAt   time.Time
	Starred     bool
	Loadable    bool
	NumMessages int
	Cost        float64
	WorkingDir  string
}

// SessionCatalog is an optional session/server-owned browsing capability.
type SessionCatalog interface {
	ListSessions(ctx context.Context) ([]SessionCatalogEntry, error)
}

// SessionSummaryOptions opts into metadata for child sessions. The zero value
// preserves the root-only scope of the historical session catalog.
type SessionSummaryOptions struct {
	IncludeChildren bool
}

// SessionSummaryEntry is a metadata-only browser row. It never contains a
// transcript; Model is the session's canonical binding, not a preview lookup.
type SessionSummaryEntry struct {
	SessionID   string    `json:"session_id"`
	ParentID    string    `json:"parent_id,omitempty"`
	Title       string    `json:"title"`
	AgentName   string    `json:"agent_name,omitempty"`
	Model       string    `json:"model,omitempty"`
	Source      string    `json:"source,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
	Starred     bool      `json:"starred"`
	NumMessages int       `json:"num_messages"`
	Cost        float64   `json:"cost"`
	WorkingDir  string    `json:"working_dir,omitempty"`
	Loaded      bool      `json:"loaded"`
	Loadable    bool      `json:"loadable"`
	// RequiresConfirmation marks a cold child candidate, not an access grant.
	// Durable membership is validated only by the confirmed session loader.
	RequiresConfirmation bool   `json:"requires_confirmation,omitempty"`
	RouteError           string `json:"route_error,omitempty"`
}

// SessionSummaryCatalog is the additive, read-only catalog surface for clients
// that must not hydrate, restore or attach sessions while browsing.
type SessionSummaryCatalog interface {
	ListSessionSummaries(ctx context.Context, options SessionSummaryOptions) ([]SessionSummaryEntry, error)
}

// SessionLoader restores/attaches a catalog row under a caller context.
type SessionLoader interface {
	LoadSession(ctx context.Context, sessionID string) (SessionHandle, *session.Session, error)
}

// SessionViewPreparer prepares a confirmed, non-executing session attachment.
type SessionViewPreparer interface {
	PrepareSessionView(ctx context.Context, sessionID string) (PreparedSessionView, error)
}

// PreparedSessionView owns only unpublished preparation resources. Commit runs
// asynchronously; Abort never revokes canonical owners after publication.
type PreparedSessionView interface {
	Info() PreparedSessionViewInfo
	Commit(ctx context.Context) (CommittedSessionView, error)
	Abort()
}

type PreparedSessionViewInfo struct {
	SessionID     string              `json:"session_id"`
	RootSessionID string              `json:"root_session_id"`
	Session       *session.Session    `json:"session"`
	Binding       SessionBinding      `json:"binding"`
	WorkingDir    string              `json:"working_dir"`
	Attach        *SubagentAttachInfo `json:"attach,omitempty"`
}

type CommittedSessionView struct {
	SessionHandle SessionHandle
	Info          PreparedSessionViewInfo
}

// AgentSwitcher clones a top-level conversation into a fresh immutable
// session identity bound to targetAgent. The source session/history remains.
type AgentSwitcher interface {
	SwitchAgent(ctx context.Context, sessionID, targetAgent string) (SessionHandle, *session.Session, error)
}

// TreeInspector reads durable topology without restoring or publishing
// sessions. A nil snapshot means the root has no persisted tree.
type TreeInspector interface {
	InspectSessionTree(ctx context.Context, rootSessionID string) (*subagent.Snapshot, error)
}

// TreeRestorer is an optional borrowed capability for runtimes whose
// persisted child sessions must be reconstructed from a durable root tree.
type TreeRestorer interface {
	RestoreSessionTree(ctx context.Context, root *session.Session) error
}

// SafetyDefaults exposes the author-declared safety mode for a session's
// bound agent (the agent's own `safety`, then the config-wide `runtime.safety`)
// so a server creating a session on a client's behalf can seed it the way the
// CLI does for fresh local sessions. It is a default only: an explicit client
// choice always wins.
type SafetyDefaults interface {
	AuthorSafetyDefault(sess *session.Session) session.SafetyPolicy
}

// SessionRuntimeSupervisor owns one session runtime. Runtime returns a restricted
// borrowed view whose concrete value does not expose Shutdown.
type SessionRuntimeSupervisor interface {
	Runtime() SessionRuntime
	Shutdown(ctx context.Context) error
}

// SessionHandle is the complete session-owned operation surface consumers retain
// instead of a concrete LocalRuntime. SessionCapabilities is the read-only
// discovery surface for optional affordances; unsupported operations return a
// typed SessionError.
type SessionHandle interface {
	ID() string
	AgentName() string
	Metadata() SessionMetadata
	Submit(ctx context.Context, input TurnInput) (Submission, error)
	Retry(ctx context.Context) (Submission, error)
	Steer(ctx context.Context, input TurnInput) (Submission, error)
	Observe(ctx context.Context, options ObserveOptions) (Observation, error)
	Status(ctx context.Context) (SessionStatus, error)
	Respond(ctx context.Context, response InteractionResponse) error
	UpdateTitle(ctx context.Context, title string) error
	Cancel(ctx context.Context, turnID string) (CancelResult, error)
	AwaitTurn(ctx context.Context, turnID string) error
	Edit(ctx context.Context, edit SessionEdit) (*session.Session, error)
	Release(ctx context.Context) error
	Snapshot(ctx context.Context) (*session.Session, error)
	Todos(ctx context.Context) ([]session.Todo, error)
	SetTodoStatus(ctx context.Context, id, status string) ([]session.Todo, error)
	SetTodoDescription(ctx context.Context, id, expectedDescription, description string) ([]session.Todo, error)
	RemoveTodo(ctx context.Context, id string) ([]session.Todo, error)
	Compact(ctx context.Context, additionalPrompt string, sink EventSink) error
	CompactTarget(ctx context.Context, sessionID, additionalPrompt string, sink EventSink) error
	ContextBreakdown(ctx context.Context) (*ContextBreakdown, error)
	LiveSessions(ctx context.Context) ([]LiveSession, error)
	Skills(ctx context.Context) ([]skills.Skill, error)
	ResolveSkillCommand(ctx context.Context, input string) (string, error)
	RunSkillFork(ctx context.Context, args skillstool.RunSkillArgs, sink EventSink) (*tools.ToolCallResult, error)
	StartSkillFork(ctx context.Context, operationID string, args skillstool.RunSkillArgs) error
	TogglePause(ctx context.Context) (bool, error)
	RefreshModelsCatalog(ctx context.Context) error
	SetStarred(ctx context.Context, starred bool) error
	RemoveAttachment(ctx context.Context, path string) error
	AvailableModels(ctx context.Context) []ModelChoice
	SetModel(ctx context.Context, modelRef string) error
	CycleThinkingLevel(ctx context.Context) (effort.Level, error)
	SetThinkingLevel(ctx context.Context, level effort.Level) (effort.Level, error)
	ThinkingLevels(ctx context.Context) []effort.Level
	CurrentThinkingLevel(ctx context.Context) effort.Level
	EmitPinnedAgentInfo(ctx context.Context, sink EventSink)
}

// UnsupportedSessionOperation returns the typed unsupported error required by
// the SessionHandle capability contract.
func UnsupportedSessionOperation(sessionID string, operation SessionOperation) error {
	return sessionUnsupported(sessionID, operation)
}

// UnsupportedSessionHandle supplies safe defaults for session operations that
// an implementation does not support. It is intentionally zero-size and may be
// embedded by value in lightweight handles and test fakes.
type UnsupportedSessionHandle struct{}

func (UnsupportedSessionHandle) Snapshot(context.Context) (*session.Session, error) {
	return nil, sessionUnsupported("", SessionOperationSource)
}

func (UnsupportedSessionHandle) Todos(context.Context) ([]session.Todo, error) {
	return nil, sessionUnsupported("", SessionOperationTodos)
}

func (UnsupportedSessionHandle) SetTodoStatus(context.Context, string, string) ([]session.Todo, error) {
	return nil, sessionUnsupported("", SessionOperationSetTodoStatus)
}

func (UnsupportedSessionHandle) RemoveTodo(context.Context, string) ([]session.Todo, error) {
	return nil, sessionUnsupported("", SessionOperationRemoveTodo)
}

func (UnsupportedSessionHandle) Compact(context.Context, string, EventSink) error {
	return sessionUnsupported("", SessionOperationCompact)
}

func (UnsupportedSessionHandle) CompactTarget(context.Context, string, string, EventSink) error {
	return sessionUnsupported("", SessionOperationCompactTarget)
}

func (UnsupportedSessionHandle) ContextBreakdown(context.Context) (*ContextBreakdown, error) {
	return nil, sessionUnsupported("", SessionOperationContext)
}

func (UnsupportedSessionHandle) LiveSessions(context.Context) ([]LiveSession, error) {
	return nil, sessionUnsupported("", SessionOperationLiveSessions)
}

func (UnsupportedSessionHandle) Skills(context.Context) ([]skills.Skill, error) {
	return nil, nil
}

func (UnsupportedSessionHandle) ResolveSkillCommand(context.Context, string) (string, error) {
	return "", nil
}

func (UnsupportedSessionHandle) RunSkillFork(context.Context, skillstool.RunSkillArgs, EventSink) (*tools.ToolCallResult, error) {
	return nil, sessionUnsupported("", SessionOperationRunSkill)
}

func (UnsupportedSessionHandle) StartSkillFork(context.Context, string, skillstool.RunSkillArgs) error {
	return sessionUnsupported("", SessionOperationRunSkill)
}

func (UnsupportedSessionHandle) TogglePause(context.Context) (bool, error) {
	return false, sessionUnsupported("", SessionOperationPause)
}

func (UnsupportedSessionHandle) RefreshModelsCatalog(context.Context) error {
	return sessionUnsupported("", SessionOperationRefreshModels)
}

func (UnsupportedSessionHandle) SetStarred(context.Context, bool) error {
	return sessionUnsupported("", SessionOperationSetStarred)
}

func (UnsupportedSessionHandle) RemoveAttachment(context.Context, string) error {
	return sessionUnsupported("", SessionOperationRemoveAttachment)
}

func (UnsupportedSessionHandle) AvailableModels(context.Context) []ModelChoice { return nil }

func (UnsupportedSessionHandle) SetModel(context.Context, string) error {
	return sessionUnsupported("", SessionOperationSetModel)
}

func (UnsupportedSessionHandle) CycleThinkingLevel(context.Context) (effort.Level, error) {
	return "", sessionUnsupported("", SessionOperationThinkingLevel)
}

func (UnsupportedSessionHandle) SetThinkingLevel(context.Context, effort.Level) (effort.Level, error) {
	return "", sessionUnsupported("", SessionOperationThinkingLevel)
}

func (UnsupportedSessionHandle) ThinkingLevels(context.Context) []effort.Level     { return nil }
func (UnsupportedSessionHandle) CurrentThinkingLevel(context.Context) effort.Level { return "" }
func (UnsupportedSessionHandle) EmitPinnedAgentInfo(context.Context, EventSink)    {}

const (
	SessionErrorInvalid      SessionErrorKind = "invalid"
	SessionErrorNotFound     SessionErrorKind = "not_found"
	SessionErrorCapacity     SessionErrorKind = "capacity"
	SessionErrorStopped      SessionErrorKind = "stopped"
	SessionErrorClosed       SessionErrorKind = "closed"
	SessionErrorStale        SessionErrorKind = "stale"
	SessionErrorUnsupported  SessionErrorKind = "unsupported"
	SessionErrorWrongSession SessionErrorKind = "wrong_session"
)

// SessionBinding pins immutable execution identity at creation.
type SessionBinding struct {
	AgentName       string
	Model           string
	Durability      subagent.Durability
	ParentSessionID string
}

// SessionCapabilities are read-only facts; false capabilities must return a
// typed unsupported error rather than silently mutating shared runtime state.
type SessionCapabilities struct {
	AvailableModels     []string
	Durability          subagent.Durability
	Compaction          bool
	TargetCompaction    bool
	ModelSwitching      bool
	ContextInspection   bool
	LiveSessions        bool
	SessionEditing      bool
	ForkSkills          bool
	Pause               bool
	ModelCatalogRefresh bool
	ThinkingLevels      bool
	Todos               bool
}

// TurnInput describes one requested turn.
type TurnInput struct {
	Content      string
	MultiContent []chat.MessagePart
	Retry        bool
	RequestID    string `json:"request_id,omitempty"`
}

// SessionMetadata is immutable handle metadata.
type SessionMetadata struct {
	SessionID      string
	AgentName      string
	Model          string
	ThinkingLevels []effort.Level
	ThinkingLevel  effort.Level
	Capabilities   SessionCapabilities
}

type SessionError struct {
	Detail    string `json:"detail,omitempty"`
	Kind      SessionErrorKind
	SessionID string
	RequestID string
	Operation SessionOperation
	Reason    SessionErrorReason `json:"reason,omitempty"`
	Limit     int
}

func (e *SessionError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("session %s: %s: %s", e.Operation, e.Kind, e.Detail)
	}
	if e.Kind == SessionErrorCapacity {
		switch e.normalizedReason() {
		case SessionErrorReasonBusy:
			return fmt.Sprintf("session %s: operation is busy", e.Operation)
		case SessionErrorReasonPending:
			return fmt.Sprintf("session %s: operation has pending input", e.Operation)
		case SessionErrorReasonLimit:
			label := "capacity"
			if e.Operation == SessionOperationActiveDescendants || e.Operation == SessionOperationActiveDescendantsRoot {
				label = "active/running capacity"
			}
			return fmt.Sprintf("session %s: %s limit reached (limit %d)", e.Operation, label, e.Limit)
		}
	}
	return fmt.Sprintf("session %s: %s", e.Operation, e.Kind)
}

func (e *SessionError) normalizedReason() SessionErrorReason {
	if e.Reason != "" || e.Kind != SessionErrorCapacity {
		return e.Reason
	}
	// Older peers encoded capacity semantics in the operation name.
	switch e.Operation {
	case SessionOperationCompactBusy, SessionOperationCompactPending, SessionOperationSkillBusy, SessionOperationSwitchAgent, SessionOperationRunSkill, SessionOperationStartGate:
		return SessionErrorReasonBusy
	default:
		return SessionErrorReasonLimit
	}
}

func (e *SessionError) Is(target error) bool {
	other, ok := target.(*SessionError)
	return ok && (other.Kind == "" || e.Kind == other.Kind)
}

var (
	ErrSessionCapacity = &SessionError{Kind: SessionErrorCapacity}
	ErrSessionStopped  = &SessionError{Kind: SessionErrorStopped}
	ErrSessionClosed   = &SessionError{Kind: SessionErrorClosed}
)

// SessionState is the session-owned lifecycle state exposed to consumers.
type SessionState string

const (
	SessionStateQueued     SessionState = "queued"
	SessionStateRunning    SessionState = "running"
	SessionStateCancelling SessionState = "cancelling"
	SessionStateSettled    SessionState = "settled"
)

// SessionStatus is a point-in-time, immutable status reading.
type SessionStatus struct {
	SessionID       string       `json:"session_id"`
	AgentName       string       `json:"agent_name"`
	State           SessionState `json:"state"`
	Pending         int          `json:"pending"`
	TurnID          string       `json:"turn_id,omitempty"`
	LastError       string       `json:"last_error,omitempty"`
	Dormant         bool         `json:"dormant,omitempty"`
	PauseArmed      bool         `json:"pause_armed,omitempty"`
	Paused          bool         `json:"paused,omitempty"`
	PauseGeneration uint64       `json:"pause_generation,omitempty"`
}

// SubmissionDisposition describes how an accepted input will be delivered.
type SubmissionDisposition string

const (
	SubmissionDispositionQueued SubmissionDisposition = "queued"
)

// Submission identifies one immutable server-assigned execution turn.
type Submission struct {
	SessionID   string                `json:"session_id"`
	TurnID      string                `json:"turn_id"`
	Disposition SubmissionDisposition `json:"disposition,omitempty"`
}

type CancelOutcome string

const (
	CancelAccepted          CancelOutcome = "accepted"
	CancelAlreadyCancelling CancelOutcome = "already_cancelling"
	CancelNotActive         CancelOutcome = "not_active"
)

type CancelResult struct {
	SessionID string        `json:"session_id"`
	TurnID    string        `json:"turn_id"`
	Outcome   CancelOutcome `json:"outcome"`
}

// InteractionKind identifies a correlated session interaction.
type InteractionKind string

const (
	InteractionConfirmation  InteractionKind = "confirmation"
	InteractionMaxIterations InteractionKind = "max_iterations"
	InteractionElicitation   InteractionKind = "elicitation"
)

// InteractionResponse resolves exactly one request belonging to this handle.
type InteractionResponse struct {
	InteractionID string
	Kind          InteractionKind
	Resume        ResumeRequest
	ElicitationID string
	Elicitation   ElicitationResult
	ClientID      string `json:"client_id,omitempty"`
}

// ObserveOptions selects replay after Since. A nil cursor means snapshot plus
// tail from the observation boundary. Buffer <= 0 uses the runtime default.
type ObserveOptions struct {
	Since  *uint64
	Buffer int
	Tree   bool
}

// InteractionSnapshot is an immutable outstanding interaction. Event is
// the original confirmation/max-iterations/elicitation payload.
type InteractionSnapshot struct {
	SessionID     string
	InteractionID string
	Kind          InteractionKind
	ElicitationID string
	Event         Event
}

// PendingInput is one accepted, durable input awaiting FIFO promotion.
type PendingInput struct {
	InputOrigin     session.InputOrigin `json:"input_origin,omitempty"`
	SenderID        string              `json:"sender_id,omitempty"`
	SenderName      string              `json:"sender_name,omitempty"`
	InputMode       string              `json:"input_mode,omitempty"`
	TurnID          string
	Content         string
	MultiContent    []chat.MessagePart
	SessionPosition int
}

// SessionSnapshot is captured atomically with observation registration. Session
// is a clone and therefore safe for consumers to retain. Interactions reseed
// outstanding prompts after reconnect or a bounded-journal gap.
type SessionSnapshot struct {
	Session            *session.Session
	Status             SessionStatus
	Interactions       []InteractionSnapshot
	PendingInputs      []PendingInput
	Cursor             uint64
	TranscriptPosition int
}

// SessionEvent is the canonical session event stream. Sequence is
// monotonically increasing per SessionID. Gap is explicit when bounded replay
// cannot satisfy the requested cursor. Observer cancellation only closes this
// observation and never cancels session execution.
type SessionEvent struct {
	Version            int
	SessionID          string
	TurnID             string
	InteractionID      string
	Sequence           uint64
	TranscriptPosition int
	Event              Event
	Gap                bool
	FirstAvailable     uint64
}

// IsLiveSeed identifies an authoritative nil-cursor snapshot supplement, not a journal event.
func (envelope SessionEvent) IsLiveSeed() bool {
	if envelope.Version == 0 || envelope.SessionID == "" || envelope.Sequence != 0 || envelope.Gap || envelope.FirstAvailable != 0 || envelope.TurnID != "" || envelope.InteractionID != "" || envelope.TranscriptPosition != -1 {
		return false
	}
	switch event := envelope.Event.(type) {
	case *StreamStartedEvent:
		return event.SessionID == envelope.SessionID
	case *AgentChoiceEvent:
		return event.SessionID == envelope.SessionID && event.Content != ""
	case *AgentChoiceReasoningEvent:
		return event.SessionID == envelope.SessionID && event.Content != ""
	case *PartialToolCallEvent:
		return event.ToolCall.ID != ""
	case *ToolCallEvent:
		return event.ToolCall.ID != ""
	case *ToolCallOutputEvent:
		return event.ToolCallID != "" && event.Output != ""
	default:
		return false
	}
}

// Observation contains an initial snapshot, ordered replay, and a live
// tail. Cancel is idempotent and does not stop the session. A Gap envelope is a
// resnapshot barrier: consumers must discard their projection and re-observe
// without a cursor; no later tail may be applied to stale projected state.
type Observation struct {
	Initial       []SessionSnapshot
	SessionsAdded <-chan SessionSnapshot
	Replay        []SessionEvent
	Events        <-chan SessionEvent
	// Errors reports terminal observation failures (HTTP/SSE parse, protocol,
	// reconnect, or unexpected EOF). It closes with the event stream. A nil
	// channel means the in-process observation cannot fail independently.
	Errors <-chan error
	Cancel func()
}

// Primary returns the observed session snapshot, or the zero value when the
// observation is malformed or unsuccessful.
func (o Observation) Primary() SessionSnapshot {
	if len(o.Initial) == 0 {
		return SessionSnapshot{}
	}
	return o.Initial[0]
}

func (UnsupportedSessionHandle) AwaitTurn(context.Context, string) error {
	return sessionUnsupported("", "await_turn")
}

func (UnsupportedSessionHandle) Edit(context.Context, SessionEdit) (*session.Session, error) {
	return nil, sessionUnsupported("", "edit")
}

func (UnsupportedSessionHandle) SetTodoDescription(context.Context, string, string, string) ([]session.Todo, error) {
	return nil, sessionUnsupported("", SessionOperationSetTodoDescription)
}
