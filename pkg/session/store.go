package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
)

type TemporaryError struct{ Err error }

func (e *TemporaryError) Error() string   { return e.Err.Error() }
func (e *TemporaryError) Unwrap() error   { return e.Err }
func (e *TemporaryError) Temporary() bool { return true }

func IsTemporary(err error) bool {
	// An explicit store/runtime wrapper can mark its own expired attempt as
	// retryable while the operation's caller context is still alive.
	var explicit *TemporaryError
	if errors.As(err, &explicit) {
		return true
	}
	// context.DeadlineExceeded implements Temporary, but a caller's expired
	// deadline is cancellation, not permission to start another attempt.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

func classifySQLiteError(err error) error {
	if err == nil || IsTemporary(err) {
		return err
	}
	// Keep the generic session surface independent of a concrete SQLite
	// implementation. SQLite drivers expose Code on their errors; the low byte
	// is the stable primary result code (5=BUSY, 6=LOCKED).
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 5, 6:
			return &TemporaryError{Err: err}
		}
	}
	return err
}

// classifySQLiteContextError preserves cancellation when a driver reports BUSY
// or INTERRUPT while returning from a canceled SQLite call. A successful commit
// stays successful even if the context is canceled just after it returns.
func classifySQLiteContextError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		var coded interface{ Code() int }
		if errors.As(err, &coded) {
			switch coded.Code() & 0xff {
			case 5, 6, 9: // BUSY, LOCKED, INTERRUPT
				return ctx.Err()
			}
		}
		if errors.Is(err, sql.ErrTxDone) {
			return ctx.Err()
		}
	}
	return classifySQLiteError(err)
}

var (
	ErrEmptyID         = errors.New("session ID cannot be empty")
	ErrNotFound        = errors.New("session not found")
	ErrAlreadyExists   = errors.New("session already exists")
	ErrOriginMismatch  = errors.New("session origin cannot be changed")
	ErrNewerDatabase   = errors.New("session database was created by a newer version of docker-agent")
	ErrMigrationFailed = errors.New("session database migration failed; existing data preserved")
)

// IsRelativeSessionRef reports whether ref is a relative session reference
// (e.g., "-1", "-2"). Explicit IDs (anything else, including UUIDs) return
// false. Callers use this to distinguish a user-supplied concrete ID — which
// may legitimately not exist yet — from a relative offset that must resolve
// against existing sessions.
func IsRelativeSessionRef(ref string) bool {
	_, isRelative := parseRelativeSessionRef(ref)
	return isRelative
}

// parseRelativeSessionRef checks if ref is a relative session reference (e.g., "-1", "-2")
// and returns the offset and whether it's a relative reference.
// Returns (1, true) for "-1", (2, true) for "-2", etc.
// Returns (0, false) if not a relative reference.
func parseRelativeSessionRef(ref string) (offset int, isRelative bool) {
	if !strings.HasPrefix(ref, "-") {
		return 0, false
	}

	// Try to parse as negative integer
	n, err := strconv.Atoi(ref)
	if err != nil || n >= 0 {
		return 0, false
	}

	return -n, true
}

// ResolveSessionID resolves a session reference to an actual session ID.
// Supports relative references like "-1" (last session), "-2" (second to last), etc.
// If the reference is not relative, it returns the input unchanged.
func ResolveSessionID(ctx context.Context, store Store, ref string) (string, error) {
	offset, isRelative := parseRelativeSessionRef(ref)
	if !isRelative {
		return ref, nil
	}

	summaries, err := store.GetSessionSummaries(ctx)
	if err != nil {
		return "", fmt.Errorf("getting session summaries: %w", err)
	}

	index := offset - 1
	if index >= len(summaries) {
		return "", fmt.Errorf("session offset %d out of range (have %d sessions)", offset, len(summaries))
	}

	return summaries[index].ID, nil
}

// Summary contains lightweight session metadata for listing purposes.
// This is used instead of loading full Session objects with all messages.
type Summary struct {
	ID                  string
	Title               string
	CreatedAt           time.Time
	Starred             bool
	NumMessages         int
	Cost                float64
	WorkingDir          string
	Attributes          map[string]string
	ParentID            string
	AgentModelOverrides map[string]string
}

// SummaryScope explicitly opts into child rows; the zero scope remains root-only.
type SummaryScope struct {
	IncludeChildren bool
}

// ScopedSummaryStore is an optional metadata-only catalog capability. It does
// not load message bodies or change the historical Store listing contract.
type ScopedSummaryStore interface {
	GetSessionSummariesWithScope(ctx context.Context, scope SummaryScope) ([]Summary, error)
}

// Store defines the interface for session storage
type Store interface {
	// === Core session operations ===
	AddSession(ctx context.Context, session *Session) error
	// GetSession retrieves a session by ID.
	GetSession(ctx context.Context, id string) (*Session, error)
	// GetSessionByOrigin retrieves a session by ID only when it belongs to origin.
	GetSessionByOrigin(ctx context.Context, id, origin string) (*Session, error)
	GetSessions(ctx context.Context) ([]*Session, error)
	GetSessionSummaries(ctx context.Context) ([]Summary, error)
	DeleteSession(ctx context.Context, id string) error
	UpdateSession(ctx context.Context, session *Session) error // Updates metadata only (not messages/items)
	SetSessionStarred(ctx context.Context, id string, starred bool) error

	// === Granular item operations ===

	// AddMessage adds a message to a session at the next position.
	// Returns the ID of the created message item.
	AddMessage(ctx context.Context, sessionID string, msg *Message) (int64, error)
	// PromotePendingUserMessage atomically clears pending session state and moves
	// the correlated input to the durable transcript tail before execution.
	PromotePendingUserMessage(ctx context.Context, sessionID, turnID string) error

	// UpdateMessage updates a message belonging to sessionID by its ID.
	// This is called on each streaming delta to keep the persisted message
	// in sync with the in-progress content, and once more with the final
	// payload when the message completes.
	UpdateMessage(ctx context.Context, sessionID string, messageID int64, msg *Message) error

	// AddSubSession creates a sub-session and links it to the parent.
	// The sub-session is stored as a separate session row with parent_id set.
	AddSubSession(ctx context.Context, parentSessionID string, subSession *Session) error

	// PersistCompaction atomically upserts session metadata and its summary item.
	// A missing row is created (matching UpdateSession); an existing row is only
	// updated when its origin matches. Implementations must apply resulting cost
	// and must not append twice when session aliases the stored live object.
	PersistCompaction(ctx context.Context, session *Session, inputTokens, outputTokens int64, item Item) error

	// AddSummary adds a summary item to a session at the next position.
	// item.FirstKeptEntry is the index of the first message kept verbatim during
	// compaction; item.Cost/Model/Usage attribute the summary's spend (zero
	// values when nothing was billed).
	AddSummary(ctx context.Context, sessionID string, item Item) error

	// AddError appends a recorded error item to a session at the next position.
	// Persisting failures lets them survive a reload and travel with a JSON export.
	AddError(ctx context.Context, sessionID string, e *Error) error

	// === Granular metadata updates ===

	// UpdateSessionTokens updates only token/cost fields
	UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error

	// UpdateSessionTitle updates only the title
	UpdateSessionTitle(ctx context.Context, sessionID, title string) error

	// Close releases any resources held by the store (e.g., database connections).
	Close() error
}

type InMemorySessionStore struct {
	coordinationMu sync.Mutex
	appendReceipts map[itemAppendKey]itemAppendReceipt
	batchReceipts  map[itemAppendKey]itemBatchReceipt
	children       map[string]ChildRecord
	reports        map[string]acceptedChildReport
	sessions       *concurrent.Map[string, *Session]
	generatedFiles *concurrent.Map[string, GeneratedFile] // keyed by generatedFileKey
	generatedBlobs *concurrent.Map[string, []byte]        // keyed by generatedFileKey
	messageID      atomic.Int64                           // counter for message IDs, incremented via Add(1)
	todos          *concurrent.Map[string, []Todo]
	todosMu        sync.Mutex
}

func NewInMemorySessionStore() Store {
	return &InMemorySessionStore{
		sessions:       concurrent.NewMap[string, *Session](),
		generatedFiles: concurrent.NewMap[string, GeneratedFile](),
		generatedBlobs: concurrent.NewMap[string, []byte](),
		todos:          concurrent.NewMap[string, []Todo](),
	}
}

func (s *InMemorySessionStore) AddSession(ctx context.Context, session *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session.ID == "" {
		return ErrEmptyID
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if _, exists := s.sessions.Load(session.ID); exists {
		return ErrAlreadyExists
	}
	return s.importSession(session.Clone())
}

func (s *InMemorySessionStore) importSession(snapshot *Session) error {
	if existing, exists := s.sessions.Load(snapshot.ID); exists {
		if existing.ParentID != snapshot.ParentID {
			return fmt.Errorf("sub-session %q belongs to a different parent", snapshot.ID)
		}
		return nil
	}
	for i, item := range snapshot.Messages {
		if item.Message != nil {
			snapshot.Messages[i].Message.ID = s.messageID.Add(1)
		}
		if item.SubSession != nil {
			item.SubSession.ParentID = snapshot.ID
			if err := s.importSession(item.SubSession); err != nil {
				return err
			}
			snapshot.Messages[i].SubSession = &Session{ID: item.SubSession.ID, ParentID: snapshot.ID}
		}
	}
	s.sessions.Store(snapshot.ID, snapshot)
	return nil
}

func (s *InMemorySessionStore) GetSession(_ context.Context, id string) (*Session, error) {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if id == "" {
		return nil, ErrEmptyID
	}
	session, exists := s.sessions.Load(id)
	if !exists {
		return nil, ErrNotFound
	}
	return s.materializeSession(session), nil
}

func (s *InMemorySessionStore) GetSessionByOrigin(_ context.Context, id, origin string) (*Session, error) {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if id == "" {
		return nil, ErrEmptyID
	}
	session, exists := s.sessions.Load(id)
	if !exists || session.Origin != origin {
		return nil, ErrNotFound
	}
	return s.materializeSession(session), nil
}

func (s *InMemorySessionStore) GetSessions(_ context.Context) ([]*Session, error) {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	sessions := make([]*Session, 0, s.sessions.Length())
	s.sessions.Range(func(key string, value *Session) bool {
		if value.ParentID == "" {
			sessions = append(sessions, s.materializeSession(value))
		}
		return true
	})
	return sessions, nil
}

func (s *InMemorySessionStore) GetSessionSummaries(ctx context.Context) ([]Summary, error) {
	return s.GetSessionSummariesWithScope(ctx, SummaryScope{})
}

func (s *InMemorySessionStore) GetSessionSummariesWithScope(ctx context.Context, scope SummaryScope) ([]Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	summaries := make([]Summary, 0, s.sessions.Length())
	s.sessions.Range(func(_ string, value *Session) bool {
		if !scope.IncludeChildren && value.ParentID != "" {
			return true
		}
		_, _, cost := value.TokensAndCost()
		overrides, _ := value.ModelStateSnapshot()
		summaries = append(summaries, Summary{
			ID:                  value.ID,
			Title:               value.TitleSnapshot(),
			CreatedAt:           value.CreatedAt,
			Starred:             value.Starred,
			NumMessages:         value.MessageCount(),
			Cost:                cost,
			WorkingDir:          value.WorkingDir,
			Attributes:          value.AttributesSnapshot(),
			ParentID:            value.ParentID,
			AgentModelOverrides: overrides,
		})
		return true
	})
	slices.SortFunc(summaries, func(a, b Summary) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return summaries, nil
}

func (s *InMemorySessionStore) DeleteSession(_ context.Context, id string) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if id == "" {
		return ErrEmptyID
	}
	_, exists := s.sessions.Load(id)
	if !exists {
		return ErrNotFound
	}
	for reportID, report := range s.reports {
		child := s.children[report.report.ChildSessionID]
		if report.report.ParentSessionID == id || report.report.ChildSessionID == id || child.RootSessionID == id {
			delete(s.reports, reportID)
		}
	}
	for childID, child := range s.children {
		if childID == id || child.ParentSessionID == id || child.RootSessionID == id {
			delete(s.children, childID)
		}
	}

	for key := range s.batchReceipts {
		if key.sessionID == id {
			delete(s.batchReceipts, key)
		}
	}
	for key := range s.appendReceipts {
		if key.sessionID == id {
			delete(s.appendReceipts, key)
		}
	}
	s.sessions.Delete(id)
	s.deleteGeneratedFiles(id)
	s.deleteGeneratedBlobs(id)
	s.todos.Delete(id)
	return nil
}

// UpdateSession updates an existing session, or creates it if it doesn't exist (upsert).
// This enables lazy session persistence - sessions are only stored when they have content.
// Note: Like SQLite, this only stores metadata. Messages are stored separately via AddMessage.
func (s *InMemorySessionStore) UpdateSession(_ context.Context, session *Session) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if session.ID == "" {
		return ErrEmptyID
	}

	newSession := session.OwnSnapshot()
	newSession.Messages = nil

	// Preserve existing messages and reject origin changes if session already exists.
	if existing, exists := s.sessions.Load(session.ID); exists {
		existing.mu.RLock()
		if existing.Origin != newSession.Origin {
			existing.mu.RUnlock()
			return fmt.Errorf("update session %q: %w", session.ID, ErrOriginMismatch)
		}
		newSession.Messages = make([]Item, len(existing.Messages))
		copy(newSession.Messages, existing.Messages)
		existing.mu.RUnlock()
	}

	s.sessions.Store(session.ID, newSession)
	return nil
}

// SetSessionStarred sets the starred status of a session.
func (s *InMemorySessionStore) SetSessionStarred(_ context.Context, id string, starred bool) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if id == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(id)
	if !exists {
		return ErrNotFound
	}
	session.mu.Lock()
	session.Starred = starred
	session.mu.Unlock()
	s.sessions.Store(id, session)
	return nil
}

// AddMessage adds a message to a session at the next position.
// Returns the ID of the created message (for in-memory, this is a simple counter).
func (s *InMemorySessionStore) AddMessage(_ context.Context, sessionID string, msg *Message) (int64, error) {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	return s.addMessage(sessionID, msg)
}

func (s *InMemorySessionStore) addMessage(sessionID string, msg *Message) (int64, error) {
	if sessionID == "" {
		return 0, ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return 0, ErrNotFound
	}
	// Deep-copy before mutating ID. The caller's pointer may be held
	// concurrently by another goroutine (snapshotItems → cloneMessage),
	// and writing msg.ID directly races with those reads.
	stored := cloneMessage(msg)
	id := s.messageID.Add(1)
	stored.ID = id
	session.AddMessage(stored)
	return id, nil
}

func (s *InMemorySessionStore) PromotePendingUserMessage(_ context.Context, sessionID, turnID string) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	sess, ok := s.sessions.Load(sessionID)
	if !ok {
		return ErrNotFound
	}
	for position, item := range sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.Pending && item.Message.TurnID == turnID {
			sess.PromotePendingUserMessage(position)
			return nil
		}
	}
	return ErrNotFound
}

// UpdateMessage updates a message belonging to sessionID by its ID.
func (s *InMemorySessionStore) UpdateMessage(_ context.Context, sessionID string, messageID int64, msg *Message) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}

	// Create a deep copy of the message to avoid mutating the caller's pointer,
	// which may be shared with another Session object.
	updated := cloneMessage(msg)
	updated.ID = messageID

	session.mu.Lock()
	defer session.mu.Unlock()
	for i := range session.Messages {
		if session.Messages[i].Message == nil || session.Messages[i].Message.ID != messageID {
			continue
		}
		session.Messages[i].Message = updated
		return nil
	}
	return ErrNotFound
}

// AddSubSession creates a sub-session and links it to the parent.
func (s *InMemorySessionStore) AddSubSession(ctx context.Context, parentSessionID string, subSession *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if parentSessionID == "" || subSession.ID == "" {
		return ErrEmptyID
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	parent, exists := s.sessions.Load(parentSessionID)
	if !exists {
		return ErrNotFound
	}
	snapshot := subSession.Clone()
	snapshot.ParentID = parentSessionID
	if err := s.importSession(snapshot); err != nil {
		return err
	}
	for _, item := range parent.MessagesSnapshot() {
		if item.SubSession != nil && item.SubSession.ID == snapshot.ID {
			return nil
		}
	}
	parent.AddSubSession(&Session{ID: snapshot.ID, ParentID: parentSessionID})
	return nil
}

func compactionSessionSnapshot(session *Session, inputTokens, outputTokens int64, item Item) (*Session, float64) {
	snapshot := session.OwnSnapshot()
	resultingCost := session.TotalCost() + item.Cost
	snapshot.InputTokens, snapshot.OutputTokens, snapshot.Cost = inputTokens, outputTokens, resultingCost
	return snapshot, resultingCost
}

// PersistCompaction atomically reflects a successful compaction in the stored
// session and applies it to compacted. The common in-memory case stores the
// live session pointer, so the operation must append exactly once.
func (s *InMemorySessionStore) PersistCompaction(_ context.Context, compacted *Session, inputTokens, outputTokens int64, item Item) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if compacted.ID == "" {
		return ErrEmptyID
	}
	snapshot, resultingCost := compactionSessionSnapshot(compacted, inputTokens, outputTokens, item)
	stored, exists := s.sessions.Load(snapshot.ID)
	if !exists {
		compacted.applyCompaction(inputTokens, outputTokens, resultingCost, item)
		s.sessions.Store(snapshot.ID, compacted.Clone())
		return nil
	}
	if stored.Origin != snapshot.Origin {
		return fmt.Errorf("persist compaction %q: %w", snapshot.ID, ErrOriginMismatch)
	}
	if stored == compacted {
		compacted.applyCompaction(inputTokens, outputTokens, resultingCost, item)
		return nil
	}
	stored.applyCompaction(inputTokens, outputTokens, resultingCost, item)
	compacted.applyCompaction(inputTokens, outputTokens, resultingCost, item)
	return nil
}

// AddSummary adds a summary item to a session at the next position.
func (s *InMemorySessionStore) AddSummary(_ context.Context, sessionID string, item Item) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.Messages = append(session.Messages, item)
	return nil
}

// AddError appends a recorded error item to a session at the next position.
func (s *InMemorySessionStore) AddError(_ context.Context, sessionID string, e *Error) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	errCopy := *e
	session.AddError(&errCopy)
	return nil
}

// querier is an interface that abstracts *sql.DB and *sql.Tx for query operations.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *InMemorySessionStore) LoadTodos(_ context.Context, sessionID string) ([]Todo, error) {
	if sessionID == "" {
		return nil, ErrEmptyID
	}
	if _, ok := s.sessions.Load(sessionID); !ok {
		return nil, ErrNotFound
	}
	items, _ := s.todos.Load(sessionID)
	return slices.Clone(items), nil
}

func (s *InMemorySessionStore) SaveTodos(_ context.Context, sessionID string, todos []Todo) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	if _, ok := s.sessions.Load(sessionID); !ok {
		return ErrNotFound
	}
	s.todos.Store(sessionID, slices.Clone(todos))
	return nil
}

func (s *InMemorySessionStore) MutateTodos(ctx context.Context, sessionID string, fn func([]Todo) ([]Todo, error)) ([]Todo, error) {
	s.todosMu.Lock()
	defer s.todosMu.Unlock()
	items, err := s.LoadTodos(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	items, err = fn(items)
	if err != nil {
		return nil, err
	}
	if err := s.SaveTodos(ctx, sessionID, items); err != nil {
		return nil, err
	}
	return slices.Clone(items), nil
}

// SQLiteSessionStore implements Store using SQLite.
type SQLiteSessionStore struct {
	db *sql.DB
}

// sessionSelectColumns is the canonical SELECT list for the sessions table.
// The column order matches what scanSession expects; all read paths use this
// constant so that adding a column requires updating exactly one place.
const sessionSelectColumns = `id, origin, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message, max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides, custom_models_used, thinking, parent_id, instruction_context, attributes, execution_settings`

// sessionPersistedFields holds the encoded form of a Session's JSON-bearing
// columns plus the SQL representation of parent_id (nil for the empty
// string, which keeps the foreign key constraint happy).
type sessionPersistedFields struct {
	PermissionsJSON         string
	AgentModelOverridesJSON string
	CustomModelsUsedJSON    string
	InstructionContextJSON  string
	AttributesJSON          string
	ExecutionSettingsJSON   string
	ParentID                any // string or nil
}

// sessionPersistedFieldsOf marshals the JSON-bearing columns of session and
// derives the SQL parent_id value. INSERT/UPDATE call sites use this helper
// so the marshaling rules ("" / "{}" / "[]" defaults, NULL parent_id) live
// in one place.
func sessionPersistedFieldsOf(session *Session) (sessionPersistedFields, error) {
	var f sessionPersistedFields
	var err error
	f.ExecutionSettingsJSON, err = encodeExecutionSettings(session)
	if err != nil {
		return f, err
	}

	attributes := session.AttributesSnapshot()
	f.AttributesJSON = "{}"
	if len(attributes) > 0 {
		attributesBytes, err := json.Marshal(attributes)
		if err != nil {
			return f, err
		}
		f.AttributesJSON = string(attributesBytes)
	}

	if session.Permissions != nil {
		permBytes, err := json.Marshal(session.Permissions)
		if err != nil {
			return f, err
		}
		f.PermissionsJSON = string(permBytes)
	}

	f.AgentModelOverridesJSON = "{}"
	if len(session.AgentModelOverrides) > 0 {
		overridesBytes, err := json.Marshal(session.AgentModelOverrides)
		if err != nil {
			return f, err
		}
		f.AgentModelOverridesJSON = string(overridesBytes)
	}

	f.CustomModelsUsedJSON = "[]"
	if len(session.CustomModelsUsed) > 0 {
		customBytes, err := json.Marshal(session.CustomModelsUsed)
		if err != nil {
			return f, err
		}
		f.CustomModelsUsedJSON = string(customBytes)
	}

	if session.InstructionContext != nil {
		contextBytes, err := json.Marshal(session.InstructionContext)
		if err != nil {
			return f, err
		}
		f.InstructionContextJSON = string(contextBytes)
	}

	// Use NULL for empty parent_id to avoid foreign key constraint issues.
	if session.ParentID != "" {
		f.ParentID = session.ParentID
	}

	return f, nil
}

// UpdateSessionTokens updates only token/cost fields.
func (s *InMemorySessionStore) UpdateSessionTokens(_ context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	session.SetTokensAndCost(inputTokens, outputTokens, cost)
	return nil
}

// UpdateSessionTitle updates only the title.
func (s *InMemorySessionStore) UpdateSessionTitle(_ context.Context, sessionID, title string) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if sessionID == "" {
		return ErrEmptyID
	}
	session, exists := s.sessions.Load(sessionID)
	if !exists {
		return ErrNotFound
	}
	session.SetTitle(title)
	return nil
}

// Close is a no-op for in-memory stores.
func (s *InMemorySessionStore) Close() error {
	return nil
}

// NewSQLiteSessionStoreFromDB wraps an already-open *sql.DB in a session store,
// running the bootstrap schema and migrations against it. The caller retains
// ownership of db: it is not closed on error, and Store.Close() will close it
// when the store is closed.
//
// This is intended primarily for tests that want to use an in-memory database
// (sql.Open("sqlite", ":memory:")) or pre-seed a database with non-default
// state. Production callers should use sqlitestore.New. Caller-supplied pools
// must configure each connection for foreign keys, a busy timeout, and immediate
// writable transactions (modernc: _txlock=immediate) when sharing a file. A
// deferred read-then-write transaction may otherwise fail with BUSY_SNAPSHOT.
func NewSQLiteSessionStoreFromDB(ctx context.Context, db *sql.DB) (store *SQLiteSessionStore, err error) {
	defer func() { err = classifySQLiteContextError(ctx, err) }()
	if db == nil {
		return nil, errors.New("db is nil")
	}
	// Callers may supply a raw *sql.DB without sqliteutil's DSN pragmas.
	// Enforce referential integrity before creating feature side tables.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return nil, fmt.Errorf("enabling sqlite foreign keys: %w", err)
	}
	if err := setupAndMigrate(ctx, db); err != nil {
		return nil, err
	}
	return &SQLiteSessionStore{db: db}, nil
}

// setupAndMigrate creates the bootstrap sessions table (if missing) and runs
// all pending schema migrations. The bootstrap schema only declares the
// columns the very first migration expects to find; later columns are added
// by subsequent ALTER TABLE migrations.
func setupAndMigrate(ctx context.Context, db *sql.DB) error {
	_, err := execSQLiteWrite(ctx, db, `
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			messages TEXT,
			created_at TEXT
		)
	`)
	if err != nil {
		return classifySQLiteError(err)
	}

	migrationManager := NewMigrationManager(db)
	return migrationManager.InitializeMigrations(ctx)
}

// parseCreatedAt parses a created_at column value. A corrupt timestamp in a
// single row must not make every session unloadable, so parse failures are
// logged and the zero time is returned instead of an error.
func parseCreatedAt(createdAtStr string) time.Time {
	t, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		slog.Warn("Invalid created_at in session database, using zero time", "value", createdAtStr, "error", err)
		return time.Time{}
	}
	return t
}

// AddSession adds a new session to the store, including any messages
func (s *SQLiteSessionStore) AddSession(ctx context.Context, session *Session) error {
	session = session.Clone()
	if session.ID == "" {
		return ErrEmptyID
	}

	fields, err := sessionPersistedFieldsOf(session)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	// Use a transaction to insert session and its items
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, origin, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes, starred, execution_settings
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.Origin, session.ToolsApproved, string(session.SafetyPolicy), session.InputTokens, session.OutputTokens, session.Title,
		session.Cost, session.SendUserMessage, session.MaxIterations, session.WorkingDir,
		session.CreatedAt.Format(time.RFC3339), fields.PermissionsJSON, fields.AgentModelOverridesJSON,
		fields.CustomModelsUsedJSON, false, fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON, session.Starred, fields.ExecutionSettingsJSON)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	// Insert all messages into session_items
	for position, item := range session.Messages {
		if err := s.addItemTx(ctx, tx, session.ID, position, item); err != nil {
			return fmt.Errorf("adding item at position %d: %w", position, classifySQLiteContextError(ctx, err))
		}
	}

	return classifySQLiteContextError(ctx, tx.Commit())
}

// scanSession scans a single row into a Session struct.
// Note: Messages are loaded separately from session_items table.
// The thinking column is read but discarded — it is kept in the schema for
// backward compatibility with older docker-agent versions that wrote it.
func scanSession(scanner interface {
	Scan(dest ...any) error
},
) (*Session, error) {
	var (
		sess                    Session
		safetyPolicy            sql.NullString
		workingDir              sql.NullString
		permissionsJSON         sql.NullString
		parentID                sql.NullString
		agentModelOverridesJSON string
		customModelsUsedJSON    string
		instructionContextJSON  sql.NullString
		attributesJSON          sql.NullString
		executionSettingsJSON   string
		createdAtStr            string
		thinking                bool // discarded
	)

	err := scanner.Scan(
		&sess.ID, &sess.Origin, &sess.ToolsApproved, &safetyPolicy, &sess.InputTokens, &sess.OutputTokens,
		&sess.Title, &sess.Cost, &sess.SendUserMessage, &sess.MaxIterations,
		&workingDir, &createdAtStr, &sess.Starred, &permissionsJSON,
		&agentModelOverridesJSON, &customModelsUsedJSON, &thinking, &parentID, &instructionContextJSON, &attributesJSON, &executionSettingsJSON,
	)
	if err != nil {
		return nil, err
	}

	if err := decodeExecutionSettings(&sess, executionSettingsJSON); err != nil {
		return nil, err
	}
	sess.CreatedAt = parseCreatedAt(createdAtStr)
	sess.SafetyPolicy = SafetyPolicy(safetyPolicy.String)
	sess.WorkingDir = workingDir.String
	sess.ParentID = parentID.String

	if permissionsJSON.Valid && permissionsJSON.String != "" {
		sess.Permissions = &PermissionsConfig{}
		if err := json.Unmarshal([]byte(permissionsJSON.String), sess.Permissions); err != nil {
			return nil, err
		}
	}

	if agentModelOverridesJSON != "" && agentModelOverridesJSON != "{}" {
		if err := json.Unmarshal([]byte(agentModelOverridesJSON), &sess.AgentModelOverrides); err != nil {
			return nil, err
		}
	}

	if customModelsUsedJSON != "" && customModelsUsedJSON != "[]" {
		if err := json.Unmarshal([]byte(customModelsUsedJSON), &sess.CustomModelsUsed); err != nil {
			return nil, err
		}
	}

	if instructionContextJSON.Valid && instructionContextJSON.String != "" {
		sess.InstructionContext = &InstructionContextState{}
		if err := json.Unmarshal([]byte(instructionContextJSON.String), sess.InstructionContext); err != nil {
			return nil, err
		}
	}

	sess.Attributes, err = decodeAttributes(attributesJSON)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling session attributes: %w", err)
	}

	return &sess, nil
}

func decodeAttributes(value sql.NullString) (map[string]string, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	var attributes map[string]string
	if err := json.Unmarshal([]byte(value.String), &attributes); err != nil {
		return nil, err
	}
	if len(attributes) == 0 {
		return nil, nil
	}
	return attributes, nil
}

// GetSession retrieves a session by ID
func (s *SQLiteSessionStore) GetSession(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	return s.loadSession(ctx, s.db, id)
}

// sessionItemRow holds the raw data from a session_items row
type sessionItemRow struct {
	id             int64
	position       int
	itemType       string
	agentName      sql.NullString
	messageJSON    sql.NullString
	implicit       bool
	actorPending   bool
	actorAccepted  bool
	actorTurnID    string
	actorInputMode string
	inputOrigin    InputOrigin
	senderID       string
	senderName     string
	reportOutcome  ReportOutcome
	subsessionID   sql.NullString
	summaryText    sql.NullString
	firstKeptEntry int
	cost           float64
	model          string
	usageJSON      string
}

// loadSessionItems loads all items for a session from session_items.
// Used both as the public read path (q == s.db) and recursively from inside
// loadSession when resolving sub-sessions inside a transaction.
func (s *SQLiteSessionStore) loadSessionItems(ctx context.Context, q querier, sessionID string) ([]Item, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, position, item_type, agent_name, message_json, implicit, COALESCE(actor_pending, 0), COALESCE(actor_accepted, 0), COALESCE(actor_turn_id, ''), COALESCE(actor_input_mode, ''), input_origin, sender_id, sender_name, report_outcome, subsession_id, summary_text, COALESCE(first_kept_entry, 0), cost, COALESCE(model, ''), COALESCE(usage_json, '')
		 FROM session_items WHERE session_id = ? ORDER BY position`, sessionID)
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()

	// First, collect all raw row data so we can close the result set
	// before making any recursive calls (SQLite doesn't allow concurrent queries)
	var rawRows []sessionItemRow
	for rows.Next() {
		var row sessionItemRow
		if err := rows.Scan(&row.id, &row.position, &row.itemType, &row.agentName, &row.messageJSON, &row.implicit, &row.actorPending, &row.actorAccepted, &row.actorTurnID, &row.actorInputMode, &row.inputOrigin, &row.senderID, &row.senderName, &row.reportOutcome, &row.subsessionID, &row.summaryText, &row.firstKeptEntry, &row.cost, &row.model, &row.usageJSON); err != nil {
			return nil, classifySQLiteContextError(ctx, err)
		}
		rawRows = append(rawRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}

	if len(rawRows) == 0 {
		return nil, nil
	}

	// Now process the collected rows, making recursive calls as needed
	var items []Item
	for _, row := range rawRows {
		switch row.itemType {
		case "message":
			var chatMsg chat.Message
			if err := json.Unmarshal([]byte(row.messageJSON.String), &chatMsg); err != nil {
				return nil, fmt.Errorf("unmarshaling message at position %d: %w", row.position, classifySQLiteContextError(ctx, err))
			}
			items = append(items, Item{
				Message: &Message{
					ID:            row.id,
					AgentName:     row.agentName.String,
					Message:       chatMsg,
					Implicit:      row.implicit,
					Pending:       row.actorPending,
					Accepted:      row.actorAccepted,
					TurnID:        row.actorTurnID,
					InputMode:     row.actorInputMode,
					InputOrigin:   row.inputOrigin,
					SenderID:      row.senderID,
					SenderName:    row.senderName,
					ReportOutcome: row.reportOutcome,
				},
			})

		case "subsession":
			// Skip if subsession_id is NULL (can happen if the sub-session was deleted
			// and the foreign key set the reference to NULL)
			if !row.subsessionID.Valid || row.subsessionID.String == "" {
				slog.WarnContext(ctx, "Skipping subsession item with NULL reference", "session_id", sessionID, "position", row.position)
				continue
			}
			// Recursively load sub-session
			subSession, err := s.loadSession(ctx, q, row.subsessionID.String)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					// Sub-session was deleted but item reference remains (orphaned reference)
					slog.WarnContext(ctx, "Skipping orphaned subsession reference", "session_id", sessionID, "subsession_id", row.subsessionID.String)
					continue
				}
				return nil, fmt.Errorf("getting sub-session %s: %w", row.subsessionID.String, classifySQLiteContextError(ctx, err))
			}
			items = append(items, Item{SubSession: subSession})

		case "summary":
			item := Item{Summary: row.summaryText.String, FirstKeptEntry: row.firstKeptEntry, Cost: row.cost, Model: row.model}
			if row.usageJSON != "" {
				var usage chat.Usage
				if err := json.Unmarshal([]byte(row.usageJSON), &usage); err != nil {
					return nil, fmt.Errorf("unmarshaling summary usage at position %d: %w", row.position, classifySQLiteContextError(ctx, err))
				}
				item.Usage = &usage
			}
			items = append(items, item)

		case "error":
			var e Error
			if row.messageJSON.Valid && row.messageJSON.String != "" {
				if err := json.Unmarshal([]byte(row.messageJSON.String), &e); err != nil {
					return nil, fmt.Errorf("unmarshaling error at position %d: %w", row.position, classifySQLiteContextError(ctx, err))
				}
			}
			items = append(items, Item{Error: &e})

		case "termination":
			var term Termination
			if row.messageJSON.Valid && row.messageJSON.String != "" {
				if err := json.Unmarshal([]byte(row.messageJSON.String), &term); err != nil {
					return nil, fmt.Errorf("unmarshaling termination at position %d: %w", row.position, classifySQLiteContextError(ctx, err))
				}
			}
			items = append(items, Item{Termination: &term})
		}
	}

	return items, nil
}

func (s *SQLiteSessionStore) GetSessionByOrigin(ctx context.Context, id, origin string) (*Session, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	return s.loadSessionByOrigin(ctx, s.db, id, origin)
}

// loadSession retrieves a session by ID using the supplied querier.
func (s *SQLiteSessionStore) loadSession(ctx context.Context, q querier, id string) (*Session, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+sessionSelectColumns+" FROM sessions WHERE id = ?", id)

	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, classifySQLiteContextError(ctx, err)
	}

	sess.Messages, err = s.loadSessionItems(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("loading session items: %w", classifySQLiteContextError(ctx, err))
	}

	return sess, nil
}

func (s *SQLiteSessionStore) loadSessionByOrigin(ctx context.Context, q querier, id, origin string) (*Session, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+sessionSelectColumns+" FROM sessions WHERE id = ? AND origin = ?", id, origin)

	sess, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, classifySQLiteContextError(ctx, err)
	}

	sess.Messages, err = s.loadSessionItems(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("loading session items: %w", classifySQLiteContextError(ctx, err))
	}

	return sess, nil
}

// GetSessions retrieves all root sessions (excludes sub-sessions)
func (s *SQLiteSessionStore) GetSessions(ctx context.Context) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+sessionSelectColumns+" FROM sessions WHERE parent_id IS NULL OR parent_id = '' ORDER BY created_at DESC")
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()

	// Collect sessions first to close the rows before loading items
	var sessions []*Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, classifySQLiteContextError(ctx, err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}

	// Load messages for each session
	for _, session := range sessions {
		items, err := s.loadSessionItems(ctx, s.db, session.ID)
		if err != nil {
			return nil, fmt.Errorf("loading items for session %s: %w", session.ID, classifySQLiteContextError(ctx, err))
		}
		session.Messages = items
	}

	return sessions, nil
}

// GetSessionSummaries retrieves lightweight session metadata for listing (excludes sub-sessions).
// This is much faster than GetSessions as it doesn't load message content.
func (s *SQLiteSessionStore) GetSessionSummaries(ctx context.Context) ([]Summary, error) {
	return s.GetSessionSummariesWithScope(ctx, SummaryScope{})
}

func (s *SQLiteSessionStore) GetSessionSummariesWithScope(ctx context.Context, scope SummaryScope) ([]Summary, error) {
	return s.sessionSummaries(ctx, scope, nil)
}

func (s *SQLiteSessionStore) sessionSummaries(ctx context.Context, scope SummaryScope, page *SummaryPageOptions) ([]Summary, error) {
	query := `SELECT s.id, s.title, s.created_at, s.starred, s.cost, s.working_dir, s.attributes,
		        (SELECT COUNT(*) FROM session_items si WHERE si.session_id = s.id AND si.item_type = 'message'),
		        s.parent_id, s.agent_model_overrides
		 FROM sessions s`
	if !scope.IncludeChildren {
		query += " WHERE (s.parent_id IS NULL OR s.parent_id = '')"
	}
	var args []any
	if page != nil && page.AfterID != "" {
		if scope.IncludeChildren {
			query += " WHERE "
		} else {
			query += " AND "
		}
		query += "(s.created_at < ? OR (s.created_at = ? AND s.id > ?))"
		stamp := page.AfterCreatedAt.Format(time.RFC3339)
		args = append(args, stamp, stamp, page.AfterID)
	}
	query += " ORDER BY s.created_at DESC, s.id ASC"
	if page != nil {
		query += " LIMIT ?"
		args = append(args, page.Limit+1)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()

	var summaries []Summary
	for rows.Next() {
		var (
			summary        Summary
			createdAtStr   string
			workingDir     sql.NullString
			attributesJSON sql.NullString
			parentID       sql.NullString
			overridesJSON  sql.NullString
		)
		if err := rows.Scan(&summary.ID, &summary.Title, &createdAtStr, &summary.Starred, &summary.Cost, &workingDir, &attributesJSON, &summary.NumMessages, &parentID, &overridesJSON); err != nil {
			return nil, classifySQLiteContextError(ctx, err)
		}
		summary.CreatedAt = parseCreatedAt(createdAtStr)
		summary.WorkingDir = workingDir.String
		summary.ParentID = parentID.String
		summary.AgentModelOverrides, err = decodeAttributes(overridesJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling session model overrides for %s: %w", summary.ID, classifySQLiteContextError(ctx, err))
		}
		summary.Attributes, err = decodeAttributes(attributesJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling session attributes for %s: %w", summary.ID, classifySQLiteContextError(ctx, err))
		}
		summaries = append(summaries, summary)
	}

	if err := rows.Err(); err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}

	return summaries, nil
}

// DeleteSession deletes a session by ID
func (s *SQLiteSessionStore) DeleteSession(ctx context.Context, id string) error {
	if id == "" {
		return ErrEmptyID
	}

	// These tables carry no foreign keys because media may be recorded before
	// the lazily persisted session row exists, so prune them explicitly.
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Explicit cleanup also covers caller-supplied pools without foreign keys.
	if _, err := tx.ExecContext(ctx, "DELETE FROM session_todos WHERE session_id = ?", id); err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM subagent_trees WHERE session_id = ?", id); err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM child_reports WHERE parent_session_id = ? OR child_session_id = ? OR child_session_id IN (SELECT session_id FROM child_records WHERE root_session_id = ?)`, id, id, id); err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM child_records WHERE session_id = ? OR parent_session_id = ? OR root_session_id = ?`, id, id, id); err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	result, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM generated_media_blobs WHERE session_id = ?", id); err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM generated_media_manifest WHERE session_id = ?", id); err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return classifySQLiteContextError(ctx, tx.Commit())
}

// UpdateSession updates an existing session's metadata, or creates it if it doesn't exist (upsert).
// Only metadata is modified - use AddMessage, AddSubSession, AddSummary for items.
// Messages are persisted separately via events to avoid duplication.
func (s *SQLiteSessionStore) UpdateSession(ctx context.Context, session *Session) error {
	if session.ID == "" {
		return ErrEmptyID
	}

	snapshot := session.OwnSnapshot()

	fields, err := sessionPersistedFieldsOf(snapshot)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	// Use a transaction
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Use INSERT OR REPLACE for upsert behavior - creates if not exists, updates if exists
	result, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, origin, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes, execution_settings
		)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title,
		   tools_approved = excluded.tools_approved,
		   safety_policy = excluded.safety_policy,
		   input_tokens = excluded.input_tokens,
		   output_tokens = excluded.output_tokens,
		   cost = excluded.cost,
		   send_user_message = excluded.send_user_message,
		   max_iterations = excluded.max_iterations,
		   working_dir = excluded.working_dir,
		   starred = excluded.starred,
		   permissions = excluded.permissions,
		   agent_model_overrides = excluded.agent_model_overrides,
		   custom_models_used = excluded.custom_models_used,
		   thinking = excluded.thinking,
		   parent_id = excluded.parent_id,
		   instruction_context = excluded.instruction_context,
		   execution_settings = excluded.execution_settings,
		   attributes = excluded.attributes
		 WHERE sessions.origin = excluded.origin`,
		snapshot.ID, snapshot.Origin, snapshot.ToolsApproved, string(snapshot.SafetyPolicy), snapshot.InputTokens, snapshot.OutputTokens,
		snapshot.Title, snapshot.Cost, snapshot.SendUserMessage, snapshot.MaxIterations, snapshot.WorkingDir,
		snapshot.CreatedAt.Format(time.RFC3339), snapshot.Starred, fields.PermissionsJSON, fields.AgentModelOverridesJSON,
		fields.CustomModelsUsedJSON, false, fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON, fields.ExecutionSettingsJSON)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("update session %q: %w", session.ID, ErrOriginMismatch)
	}

	// Note: Messages are NOT persisted here. They are persisted via events
	// (UserMessageEvent, MessageAddedEvent, etc.) to avoid duplication.

	return classifySQLiteContextError(ctx, tx.Commit())
}

// SetSessionStarred sets the starred status of a session.
func (s *SQLiteSessionStore) SetSessionStarred(ctx context.Context, id string, starred bool) error {
	if id == "" {
		return ErrEmptyID
	}

	result, err := execSQLiteWrite(ctx, s.db, "UPDATE sessions SET starred = ? WHERE id = ?", starred, id)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// closeDrainTimeout bounds how long Close waits for in-use connections.
const closeDrainTimeout = 5 * time.Second

// Close closes the database connection, waiting for in-flight statements to
// release it so the file can be removed immediately afterwards. sql.DB.Close
// only closes idle connections; a background persistence write still holding
// one keeps the file open on Windows until it returns.
//
// Mirrors sqliteutil.CloseDB; duplicated so pkg/session stays free of the
// SQLite driver (see pkg/session/sqlitestore).
func (s *SQLiteSessionStore) Close() error {
	err := s.db.Close()
	deadline := time.Now().Add(closeDrainTimeout)
	for s.db.Stats().OpenConnections > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return err
}

// AddMessage adds a message to a session at the next position.
// Returns the ID of the created message item.
func (s *SQLiteSessionStore) AddMessage(ctx context.Context, sessionID string, msg *Message) (int64, error) {
	if sessionID == "" {
		return 0, ErrEmptyID
	}

	msgJSON, err := json.Marshal(msg.Message)
	if err != nil {
		return 0, fmt.Errorf("marshaling message: %w", classifySQLiteContextError(ctx, err))
	}

	// Insert a new message at the next position
	result, err := execSQLiteWrite(ctx, s.db,
		`INSERT INTO session_items (session_id, position, item_type, agent_name, message_json, implicit, actor_pending, actor_accepted, actor_turn_id, actor_input_mode, input_origin, sender_id, sender_name, report_outcome)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'message', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, sessionID, msg.AgentName, string(msgJSON), msg.Implicit, msg.Pending, msg.Accepted, msg.TurnID, msg.InputMode, msg.InputOrigin, msg.SenderID, msg.SenderName, msg.ReportOutcome)
	if err != nil {
		return 0, fmt.Errorf("inserting message: %w", classifySQLiteContextError(ctx, err))
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", classifySQLiteContextError(ctx, err))
	}

	slog.DebugContext(ctx, "[STORE] AddMessage", "session_id", sessionID, "message_id", id, "role", msg.Message.Role, "agent", msg.AgentName)
	return id, nil
}

func (s *SQLiteSessionStore) PromotePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()
	var rawPosition, position, itemCount int
	if err := tx.QueryRowContext(ctx, `SELECT target.position,
		(SELECT COUNT(*) FROM session_items earlier WHERE earlier.session_id = target.session_id AND earlier.position < target.position),
		(SELECT COUNT(*) FROM session_items WHERE session_id = ?)
		FROM session_items target WHERE target.session_id = ? AND target.actor_pending = 1 AND target.actor_turn_id = ?`, sessionID, sessionID, turnID).Scan(&rawPosition, &position, &itemCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return classifySQLiteContextError(ctx, err)
	}
	// Keep summary boundaries stable under the same move-to-tail transform used
	// by Session.promotePendingUserMessageLocked.
	if _, err := tx.ExecContext(ctx, `UPDATE session_items SET first_kept_entry = first_kept_entry - 1
		WHERE session_id = ? AND summary_text IS NOT NULL AND ? < first_kept_entry AND first_kept_entry < ?`, sessionID, position, itemCount); err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_items
		SET actor_pending = 0, position = (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?)
		WHERE session_id = ? AND position = ? AND actor_pending = 1 AND actor_turn_id = ?`, sessionID, sessionID, rawPosition, turnID)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if changed != 1 {
		return ErrNotFound
	}
	return classifySQLiteContextError(ctx, tx.Commit())
}

// UpdateMessage updates a message belonging to sessionID by its ID.
func (s *SQLiteSessionStore) UpdateMessage(ctx context.Context, sessionID string, messageID int64, msg *Message) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	msgJSON, err := json.Marshal(msg.Message)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", classifySQLiteContextError(ctx, err))
	}

	result, err := execSQLiteWrite(ctx, s.db,
		`UPDATE session_items SET message_json = ?, implicit = ?, actor_pending = ?, actor_accepted = ?, actor_turn_id = ?, actor_input_mode = ?, input_origin = ?, sender_id = ?, sender_name = ?, report_outcome = ? WHERE session_id = ? AND id = ?`,
		string(msgJSON), msg.Implicit, msg.Pending, msg.Accepted, msg.TurnID, msg.InputMode, msg.InputOrigin, msg.SenderID, msg.SenderName, msg.ReportOutcome, sessionID, messageID)
	if err != nil {
		return fmt.Errorf("updating message: %w", classifySQLiteContextError(ctx, err))
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", classifySQLiteContextError(ctx, err))
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// AddSubSession creates a sub-session and links it to the parent.
func (s *SQLiteSessionStore) AddSubSession(ctx context.Context, parentSessionID string, subSession *Session) error {
	if parentSessionID == "" || subSession.ID == "" {
		return ErrEmptyID
	}

	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Persist from an immutable snapshot. AddSubSession may run while the
	// child is still winding down, and store implementations must not mutate
	// or iterate over a live session owned by the runtime.
	snapshot := subSession.Clone()
	snapshot.ParentID = parentSessionID
	if err := s.ensureSubSessionTx(ctx, tx, snapshot); err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	// Add the reference in the parent's items, once: repeat persists of the
	// same sub-session must not duplicate it in the parent's transcript.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, subsession_id)
		 SELECT ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'subsession', ?
		 WHERE NOT EXISTS (
			SELECT 1 FROM session_items WHERE session_id = ? AND item_type = 'subsession' AND subsession_id = ?
		 )`,
		parentSessionID, parentSessionID, subSession.ID, parentSessionID, subSession.ID)
	if err != nil {
		return fmt.Errorf("inserting subsession reference: %w", classifySQLiteContextError(ctx, err))
	}

	return classifySQLiteContextError(ctx, tx.Commit())
}

// upsertSubSessionTx imports a missing session and its initial history.
func (s *SQLiteSessionStore) upsertSubSessionTx(ctx context.Context, tx *sql.Tx, subSession *Session) error {
	if err := s.upsertSessionRowTx(ctx, tx, subSession); err != nil {
		return fmt.Errorf("upserting sub-session: %w", classifySQLiteContextError(ctx, err))
	}

	for i, item := range subSession.Messages {
		if err := s.addItemTx(ctx, tx, subSession.ID, i, item); err != nil {
			return fmt.Errorf("inserting sub-session item %d: %w", i, classifySQLiteContextError(ctx, err))
		}
	}
	return nil
}

// ensureSubSessionTx imports missing descendants, but never writes through an
// ancestor snapshot into a descendant that already has its own durable row.
func (s *SQLiteSessionStore) ensureSubSessionTx(ctx context.Context, tx *sql.Tx, child *Session) error {
	var parentID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT parent_id FROM sessions WHERE id = ?`, child.ID).Scan(&parentID)
	if err == nil {
		if parentID.String != child.ParentID {
			return fmt.Errorf("sub-session %q belongs to a different parent", child.ID)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return classifySQLiteContextError(ctx, err)
	}
	return s.upsertSubSessionTx(ctx, tx, child)
}

// upsertSessionRowTx inserts or updates a session row within a transaction.
// Used for sub-session snapshots, whose row may already exist from an
// earlier turn's persist or a title update.
func (s *SQLiteSessionStore) upsertSessionRowTx(ctx context.Context, tx *sql.Tx, session *Session) error {
	fields, err := sessionPersistedFieldsOf(session)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, origin, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes, execution_settings
		)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   origin = excluded.origin,
		   title = excluded.title,
		   tools_approved = excluded.tools_approved,
		   safety_policy = excluded.safety_policy,
		   input_tokens = excluded.input_tokens,
		   output_tokens = excluded.output_tokens,
		   cost = excluded.cost,
		   send_user_message = excluded.send_user_message,
		   max_iterations = excluded.max_iterations,
		   working_dir = excluded.working_dir,
		   starred = excluded.starred,
		   permissions = excluded.permissions,
		   agent_model_overrides = excluded.agent_model_overrides,
		   custom_models_used = excluded.custom_models_used,
		   parent_id = excluded.parent_id,
		   instruction_context = excluded.instruction_context,
		   execution_settings = excluded.execution_settings,
		   attributes = excluded.attributes`,
		session.ID, session.Origin, session.ToolsApproved, string(session.SafetyPolicy), session.InputTokens, session.OutputTokens,
		session.Title, session.Cost, session.SendUserMessage, session.MaxIterations,
		session.WorkingDir, session.CreatedAt.Format(time.RFC3339), session.Starred,
		fields.PermissionsJSON, fields.AgentModelOverridesJSON, fields.CustomModelsUsedJSON, false,
		fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON, fields.ExecutionSettingsJSON)
	return classifySQLiteContextError(ctx, err)
}

// addItemTx inserts a session item within a transaction.
func (s *SQLiteSessionStore) addItemTx(ctx context.Context, tx *sql.Tx, sessionID string, position int, item Item) error {
	switch {
	case item.Message != nil:
		msgJSON, err := json.Marshal(item.Message.Message)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", classifySQLiteContextError(ctx, err))
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, agent_name, message_json, implicit, actor_pending, actor_accepted, actor_turn_id, actor_input_mode, input_origin, sender_id, sender_name, report_outcome)
			 VALUES (?, ?, 'message', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, position, item.Message.AgentName, string(msgJSON), item.Message.Implicit, item.Message.Pending, item.Message.Accepted, item.Message.TurnID, item.Message.InputMode, item.Message.InputOrigin, item.Message.SenderID, item.Message.SenderName, item.Message.ReportOutcome)
		return classifySQLiteContextError(ctx, err)

	case item.SubSession != nil:
		// Nested sub-sessions are already cloned with the outer snapshot.
		item.SubSession.ParentID = sessionID
		if err := s.ensureSubSessionTx(ctx, tx, item.SubSession); err != nil {
			return fmt.Errorf("upserting nested sub-session: %w", classifySQLiteContextError(ctx, err))
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, subsession_id)
			 VALUES (?, ?, 'subsession', ?)`,
			sessionID, position, item.SubSession.ID)
		return classifySQLiteContextError(ctx, err)

	case item.Summary != "":
		usageJSON, err := summaryUsageJSON(item.Usage)
		if err != nil {
			return classifySQLiteContextError(ctx, err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, summary_text, first_kept_entry, cost, model, usage_json)
			 VALUES (?, ?, 'summary', ?, ?, ?, ?, ?)`,
			sessionID, position, item.Summary, item.FirstKeptEntry, item.Cost, item.Model, usageJSON)
		return classifySQLiteContextError(ctx, err)

	case item.Error != nil:
		errJSON, err := json.Marshal(item.Error)
		if err != nil {
			return fmt.Errorf("marshaling error: %w", classifySQLiteContextError(ctx, err))
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, message_json)
			 VALUES (?, ?, 'error', ?)`,
			sessionID, position, string(errJSON))
		return classifySQLiteContextError(ctx, err)

	case item.Termination != nil:
		// Terminations reuse the message_json column like error items;
		// item_type discriminates the row, so no schema migration is needed.
		termJSON, err := json.Marshal(item.Termination)
		if err != nil {
			return fmt.Errorf("marshaling termination: %w", classifySQLiteContextError(ctx, err))
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, message_json)
			 VALUES (?, ?, 'termination', ?)`,
			sessionID, position, string(termJSON))
		return classifySQLiteContextError(ctx, err)

	default:
		return nil // Empty item, skip
	}
}

// PersistCompaction commits the compaction metadata and summary row in one
// transaction so a reload cannot observe only half of the continuation state.
func (s *SQLiteSessionStore) PersistCompaction(ctx context.Context, compacted *Session, inputTokens, outputTokens int64, item Item) error {
	if compacted.ID == "" {
		return ErrEmptyID
	}
	usageJSON, err := summaryUsageJSON(item.Usage)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	snapshot, resultingCost := compactionSessionSnapshot(compacted, inputTokens, outputTokens, item)
	fields, err := sessionPersistedFieldsOf(snapshot)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (
			id, origin, tools_approved, safety_policy, input_tokens, output_tokens, title, cost, send_user_message,
			max_iterations, working_dir, created_at, starred, permissions, agent_model_overrides,
			custom_models_used, thinking, parent_id, instruction_context, attributes, execution_settings
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cost = excluded.cost
		WHERE sessions.origin = excluded.origin`,
		snapshot.ID, snapshot.Origin, snapshot.ToolsApproved, string(snapshot.SafetyPolicy), snapshot.InputTokens, snapshot.OutputTokens,
		snapshot.Title, snapshot.Cost, snapshot.SendUserMessage, snapshot.MaxIterations, snapshot.WorkingDir,
		snapshot.CreatedAt.Format(time.RFC3339), snapshot.Starred, fields.PermissionsJSON, fields.AgentModelOverridesJSON,
		fields.CustomModelsUsedJSON, false, fields.ParentID, fields.InstructionContextJSON, fields.AttributesJSON, fields.ExecutionSettingsJSON)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("persist compaction %q: %w", snapshot.ID, ErrOriginMismatch)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, summary_text, first_kept_entry, cost, model, usage_json)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'summary', ?, ?, ?, ?, ?)`,
		snapshot.ID, snapshot.ID, item.Summary, item.FirstKeptEntry, item.Cost, item.Model, usageJSON)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	compacted.applyCompaction(inputTokens, outputTokens, resultingCost, item)
	return nil
}

// AddSummary adds a summary item to a session at the next position.
func (s *SQLiteSessionStore) AddSummary(ctx context.Context, sessionID string, item Item) error {
	if sessionID == "" {
		return ErrEmptyID
	}

	usageJSON, err := summaryUsageJSON(item.Usage)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	_, err = execSQLiteWrite(ctx, s.db,
		`INSERT INTO session_items (session_id, position, item_type, summary_text, first_kept_entry, cost, model, usage_json)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'summary', ?, ?, ?, ?, ?)`,
		sessionID, sessionID, item.Summary, item.FirstKeptEntry, item.Cost, item.Model, usageJSON)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}

	return nil
}

// summaryUsageJSON serializes a summary item's usage for the usage_json
// column; nil usage maps to the empty string so old rows and unbilled
// summaries look the same on load.
func summaryUsageJSON(usage *chat.Usage) (string, error) {
	if usage == nil {
		return "", nil
	}
	b, err := json.Marshal(usage)
	if err != nil {
		return "", fmt.Errorf("marshaling summary usage: %w", err)
	}
	return string(b), nil
}

// AddError appends a recorded error item to a session at the next position.
// The error payload is stored as JSON in the message_json column, reusing the
// existing schema (item_type discriminates the row).
func (s *SQLiteSessionStore) AddError(ctx context.Context, sessionID string, e *Error) error {
	if sessionID == "" {
		return ErrEmptyID
	}

	errJSON, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshaling error: %w", classifySQLiteContextError(ctx, err))
	}

	_, err = execSQLiteWrite(ctx, s.db,
		`INSERT INTO session_items (session_id, position, item_type, message_json)
		 VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'error', ?)`,
		sessionID, sessionID, string(errJSON))
	return classifySQLiteContextError(ctx, err)
}

// UpdateSessionTokens updates only token/cost fields.
func (s *SQLiteSessionStore) UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	_, err := execSQLiteWrite(ctx, s.db,
		"UPDATE sessions SET input_tokens = ?, output_tokens = ?, cost = ? WHERE id = ?",
		inputTokens, outputTokens, cost, sessionID)
	return classifySQLiteContextError(ctx, err)
}

// UpdateSessionTitle updates only the title.
func (s *SQLiteSessionStore) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	_, err := execSQLiteWrite(ctx, s.db,
		"UPDATE sessions SET title = ? WHERE id = ?",
		title, sessionID)
	return classifySQLiteContextError(ctx, err)
}
