package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

type sessionRestoreLock struct {
	mu   sync.Mutex
	refs int
}

type sessionRestoreLockSet struct {
	mu    sync.Mutex
	locks map[string]*sessionRestoreLock
}

func newSessionRestoreLockSet() *sessionRestoreLockSet {
	return &sessionRestoreLockSet{locks: make(map[string]*sessionRestoreLock)}
}

func (s *sessionRestoreLockSet) lock(key string) func() {
	s.mu.Lock()
	entry := s.locks[key]
	if entry == nil {
		entry = &sessionRestoreLock{}
		s.locks[key] = entry
	}
	entry.refs++
	s.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			entry.refs--
			if entry.refs == 0 {
				delete(s.locks, key)
			}
		}()
	}
}

func (s *sessionRestoreLockSet) length() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.locks)
}

type activeRuntimes struct {
	handle   runtime.SessionHandle
	registry runtime.SessionRuntime
}

type SessionManager struct {
	runtimeSessions *concurrent.Map[string, *activeRuntimes]
	sessionStore    session.Store
	Sources         config.Sources

	runConfig *config.RuntimeConfig

	// sessionWorkingDirRoot, when non-empty, confines the user-supplied
	// working_dir of POST /api/v2/sessions to that directory (see
	// WithSessionWorkingDirRoot). It is a dedicated boundary: deriving it
	// from runConfig.WorkingDir or the process cwd broke long-lived daemons
	// that open arbitrary host workspaces and was reverted (#3788).
	sessionWorkingDirRoot string

	// sessionRegistry is the default session registry. sessionRegistries enables
	// explicit multi-source routing; session source identity is persisted in a
	// namespaced attribute and used for every later lookup.
	sessionRegistry   runtime.SessionRuntime
	sessionRegistries map[string]runtime.SessionRuntime

	// sessionRuntimeFactory, when set, builds the per-source registries above
	// lazily (see WithSessionRuntimeFactory); ownedRegistries are the routers
	// this manager built and must shut down.
	sessionRuntimeFactory SessionRuntimeFactory
	ownedRegistries       []*workspaceSessionRuntimes

	// sessionRestoreLocks serialize cold publication by durable root. Children share
	// their root's lock because restoring one child reconstructs the whole tree.
	sessionRestoreLocks *sessionRestoreLockSet

	refreshInterval time.Duration

	mux sync.Mutex

	// serverCtx owns accepted background session operations after request return.
	serverCtx context.Context //nolint:containedctx // lifecycle root intentionally owned and cancelled by this long-lived component

	// sessionReady is closed once the first session is attached or created,
	// signalling that the server is ready to accept session-scoped requests.
	sessionReady     chan struct{}
	sessionReadyOnce sync.Once
}

// SessionManagerOpt configures a SessionManager created by NewSessionManager.
type SessionManagerOpt func(*SessionManager)

// WithSessionWorkingDirRoot confines the working_dir accepted by
// CreateSession (POST /api/v2/sessions) to root: after resolving symlinks,
// the requested directory must be root or one of its descendants. Empty
// (the default) keeps the API unrestricted — the intended behaviour for
// local single-user daemons that legitimately open arbitrary host
// workspaces. Multi-user or network-exposed deployments should set a
// root (--session-workingdir-root).
func WithSessionWorkingDirRoot(root string) SessionManagerOpt {
	return func(sm *SessionManager) {
		sm.sessionWorkingDirRoot = root
	}
}

// WithSessionRuntime injects a session registry without exposing the legacy
// runtime stream façade.
func WithSessionRuntime(registry runtime.SessionRuntime) SessionManagerOpt {
	return func(sm *SessionManager) { sm.sessionRegistry = registry }
}

// WithSessionRuntimes installs explicitly named registries for multi-source
// session routing. The map is cloned so callers may safely release bootstrap
// state after construction.
func WithSessionRuntimes(registries map[string]runtime.SessionRuntime) SessionManagerOpt {
	return func(sm *SessionManager) {
		sm.sessionRegistries = maps.Clone(registries)
		if len(registries) == 1 {
			for _, registry := range registries {
				sm.sessionRegistry = registry
			}
		}
	}
}

// NewSessionManager creates a new session manager.
func NewSessionManager(ctx context.Context, sources config.Sources, sessionStore session.Store, refreshInterval time.Duration, runConfig *config.RuntimeConfig, opts ...SessionManagerOpt) *SessionManager {
	loaders := make(config.Sources)
	for name, source := range sources {
		loaders[name] = newSourceLoader(ctx, source, refreshInterval)
	}

	sm := &SessionManager{
		serverCtx:           ctx,
		runtimeSessions:     concurrent.NewMap[string, *activeRuntimes](),
		sessionRestoreLocks: newSessionRestoreLockSet(),
		sessionStore:        sessionStore,
		Sources:             loaders,
		refreshInterval:     refreshInterval,
		runConfig:           runConfig,
		sessionReady:        make(chan struct{}),
	}

	for _, opt := range opts {
		opt(sm)
	}

	if sm.sessionRuntimeFactory != nil && len(sm.sessionRegistries) == 0 {
		sm.sessionRegistries = make(map[string]runtime.SessionRuntime, len(loaders))
		for name, loader := range loaders {
			router := newWorkspaceSessionRuntimes(ctx, loader, sm.sessionRuntimeFactory, sm.sessionStore)
			sm.sessionRegistries[name] = router
			sm.ownedRegistries = append(sm.ownedRegistries, router)
			if len(loaders) == 1 {
				sm.sessionRegistry = router
			}
		}
	}

	return sm
}

// Shutdown stops every session runtime the manager built through its
// SessionRuntimeFactory. Registries injected prebuilt stay with their owners.
func (sm *SessionManager) Shutdown(ctx context.Context) error {
	var errs []error
	for _, router := range sm.ownedRegistries {
		if err := router.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (sm *SessionManager) markReady() {
	sm.sessionReadyOnce.Do(func() { close(sm.sessionReady) })
}

// WaitReady blocks until at least one session has been attached or created,
// or ctx is cancelled. Returns nil when ready, ctx.Err() on timeout.
func (sm *SessionManager) WaitReady(ctx context.Context) error {
	select {
	case <-sm.sessionReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetSession retrieves a session by ID.
func (sm *SessionManager) GetSession(ctx context.Context, id string) (*session.Session, error) {
	if rs, ok := sm.runtimeSessions.Load(id); ok && rs.handle != nil {
		return rs.handle.Snapshot(ctx)
	}
	sess, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return sess.Clone(), nil
}

// ErrInvalidWorkingDir marks a rejected client-supplied working_dir in
// CreateSession (raw "..", nonexistent or non-directory path, outside the
// configured root). Matched via errors.Is by the HTTP handler to answer
// 400 instead of 500; operator misconfiguration (a broken configured
// root) deliberately does not wrap it.
var ErrInvalidWorkingDir = errors.New("invalid working directory")

// CreateSession creates a new session from a template.
func (sm *SessionManager) CreateSession(ctx context.Context, sessionTemplate *session.Session) (*session.Session, error) {
	sess, err := sm.prepareSession(sessionTemplate)
	if err != nil {
		return nil, err
	}
	if agentName := sess.AttributesSnapshot()[sessionAgentAttribute]; agentName != "" {
		registry, _, err := sm.sessionRegistryForCreate(sess.AttributesSnapshot()[sessionSourceAttribute])
		if err != nil {
			return nil, err
		}
		handle, err := sm.createHTTPSession(ctx, registry, sess, runtime.SessionBinding{AgentName: agentName})
		if err != nil {
			return nil, err
		}
		return handle.Snapshot(ctx)
	}
	if err := sm.sessionStore.AddSession(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func (sm *SessionManager) prepareSession(sessionTemplate *session.Session) (*session.Session, error) {
	var opts []session.Opt
	opts = append(opts,
		session.WithMaxIterations(sessionTemplate.MaxIterations),
		session.WithMaxConsecutiveToolCalls(sessionTemplate.MaxConsecutiveToolCalls),
		session.WithMaxOldToolCallTokens(sessionTemplate.MaxOldToolCallTokens),
		session.WithMaxToolResultTokens(sessionTemplate.MaxToolResultTokens),
		session.WithToolsApproved(sessionTemplate.ToolsApproved),
	)
	if sessionTemplate.SafetyPolicy != "" {
		opts = append(opts, session.WithSafetyPolicy(sessionTemplate.SafetyPolicy))
	}

	// Carry a caller-supplied title (from the POST /api/v2/sessions request body)
	// into the new session. The title is persisted as supplied.
	if title := strings.TrimSpace(sessionTemplate.Title); title != "" {
		opts = append(opts, session.WithTitle(title))
	}

	// A template without a working_dir creates an intentionally
	// workspace-less session: the API caller is remote and the server must
	// not guess its own process cwd as the session's workspace provenance.
	if wd := strings.TrimSpace(sessionTemplate.WorkingDir); wd != "" {
		// Refuse any raw ".." here in CreateSession, before filepath.Abs
		// cleans it away: the traversal rejection is auditable on the raw
		// value and CodeQL recognizes the direct guard (go/path-injection).
		// Deliberately conservative — even a plain filename like "foo..bar"
		// is rejected. Absolute, already-clean paths — what callers
		// actually send — pass through untouched (#3788).
		if strings.Contains(wd, "..") {
			return nil, fmt.Errorf("%w: %q must not contain %q", ErrInvalidWorkingDir, wd, "..")
		}
		absWd, err := filepath.Abs(wd)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidWorkingDir, err)
		}
		resolvedWd, err := sm.resolveWithinRoot(absWd)
		if err != nil {
			return nil, err
		}
		// Without a configured root, resolvedWd is the caller-chosen
		// directory, untouched: the unrestricted default is intentional
		// for trusted local daemons, which open sessions on arbitrary
		// host paths (#3788). Deployments that need containment must
		// configure WithSessionWorkingDirRoot, enforced by
		// resolveWithinRoot above.
		info, err := os.Stat(resolvedWd)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidWorkingDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: %q is not a directory", ErrInvalidWorkingDir, resolvedWd)
		}
		opts = append(opts, session.WithWorkingDir(resolvedWd))
	}

	if sessionTemplate.Permissions != nil {
		opts = append(opts, session.WithPermissions(sessionTemplate.Permissions))
	}
	if attributes := sessionTemplate.AttributesSnapshot(); len(attributes) > 0 {
		opts = append(opts, session.WithAttributes(attributes))
	}

	sess := session.New(opts...)

	// Copy model-related fields from the template so callers can pin a
	// specific model when creating a session over the API. The runtime
	// will pick these up the first time it is built for the session
	// These persisted fields are retained as session metadata; immutable session
	// binding and per-turn model admission are enforced by the session API.
	if len(sessionTemplate.AgentModelOverrides) > 0 {
		sess.AgentModelOverrides = maps.Clone(sessionTemplate.AgentModelOverrides)
	}
	if len(sessionTemplate.CustomModelsUsed) > 0 {
		sess.CustomModelsUsed = append([]string(nil), sessionTemplate.CustomModelsUsed...)
	}

	return sess, nil
}

// workingDirRoot returns the absolute containment root for user-supplied
// session working directories, or "" when none was configured and the API
// is intentionally unrestricted. A configured root that trims to empty
// (e.g. an unresolved shell variable) is a misconfiguration: the operator
// asked for containment, so it fails loudly instead of silently disabling
// the protection. runConfig.WorkingDir is deliberately never consulted:
// it is a default cwd for tools, not a security boundary (#3788).
func (sm *SessionManager) workingDirRoot() (string, error) {
	if sm.sessionWorkingDirRoot == "" {
		return "", nil
	}
	root := strings.TrimSpace(sm.sessionWorkingDirRoot)
	if root == "" {
		return "", errors.New("session working-dir root is empty after trimming whitespace")
	}
	return filepath.Abs(root)
}

// resolveWithinRoot enforces the opt-in containment of user-supplied
// session working directories (go/path-injection, CodeQL alert #57).
// When a root is configured (WithSessionWorkingDirRoot), root and
// candidate are both canonicalised via filepath.EvalSymlinks and
// inclusion is checked component-wise on the filepath.Rel result — never
// a raw prefix comparison — so ".." traversal, absolute escapes and
// symlinks pointing outside the root are all rejected; the canonicalised
// path is returned for storage. Without a root, absPath is returned
// untouched: arbitrary host directories are the intended,
// backwards-compatible default, and neither the process cwd nor
// --working-dir may serve as an implicit boundary (#3788).
// Failures caused by the submitted path wrap ErrInvalidWorkingDir;
// a broken configured root does not.
func (sm *SessionManager) resolveWithinRoot(absPath string) (string, error) {
	root, err := sm.workingDirRoot()
	if err != nil {
		return "", err
	}
	if root == "" {
		return absPath, nil
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidWorkingDir, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	// IsLocal accepts "." (the root itself) and any descendant, and
	// rejects "", absolute paths and anything whose first component is
	// ".." — without misclassifying siblings like "..foo".
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: %q is outside the permitted root %q", ErrInvalidWorkingDir, absPath, root)
	}
	return resolvedPath, nil
}

// Sentinel errors returned by ForkSession. Matched via errors.Is by
// the HTTP handler to classify failures as 400 vs 500, so the messages
// can be reworded safely.
var (
	ErrForkOutOfRange   = errors.New("fork user-message index out of range")
	ErrForkInSubSession = errors.New("fork user-message index falls inside a sub-session")
)

// ForkSession creates a new session whose history is a deep copy of
// the parent session up to (but excluding) the Nth user message, with
// a fork-numbered title ("<parent> (fork N)"). userMessageOrdinal
// counts user-role messages in the flat list returned by
// Session.GetAllMessages.
//
// The read-then-write of the session store is serialised under sm.mux
// to keep two concurrent forks on the same parent from racing on the
// auto-numbered title.
func (sm *SessionManager) ForkSession(ctx context.Context, sessionID string, userMessageOrdinal int) (*session.Session, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	parent, err := sm.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if parent.ParentID != "" || parent.AttributesSnapshot()[sessionAgentAttribute] != "" {
		handle, err := sm.Handle(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		parent, err = handle.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
	}

	itemIndex, err := userMessageOrdinalToItemIndex(parent, userMessageOrdinal)
	if err != nil {
		return nil, err
	}

	forked, err := session.ForkSession(parent, itemIndex)
	if err != nil {
		return nil, err
	}

	// Sibling-aware title so repeated forks of the same parent get
	// (fork 1), (fork 2), … instead of colliding on (fork 1).
	siblings, err := sm.sessionStore.GetSessions(ctx)
	if err != nil {
		return nil, err
	}
	siblingTitles := make([]string, 0, len(siblings))
	for _, s := range siblings {
		siblingTitles = append(siblingTitles, s.TitleSnapshot())
	}
	forked.SetTitle(session.NextForkTitle(parent.TitleSnapshot(), siblingTitles))

	if parent.ParentID != "" || parent.AttributesSnapshot()[sessionAgentAttribute] != "" {
		handle, err := sm.Handle(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		registry, _, err := sm.sessionRegistryForCreate(parent.AttributesSnapshot()[sessionSourceAttribute])
		if err != nil {
			return nil, err
		}
		if active, ok := sm.runtimeSessions.Load(handle.ID()); ok {
			registry = active.registry
		}
		created, err := sm.createHTTPSession(ctx, registry, forked, runtime.SessionBinding{AgentName: handle.AgentName()})
		if err != nil {
			return nil, err
		}
		return created.Snapshot(ctx)
	}
	if err := sm.sessionStore.AddSession(ctx, forked); err != nil {
		return nil, err
	}
	return forked, nil
}

// userMessageOrdinalToItemIndex maps a 0-based user-message ordinal
// into an index in the parent's Session.Messages Item slice. Returns
// ErrForkOutOfRange or ErrForkInSubSession on invalid input.
//
// It walks a MessagesSnapshot rather than s.Messages directly: s is the
// live, shared session pointer returned by InMemorySessionStore.GetSession,
// which a concurrent HTTP AddMessage or the runtime's own compaction can
// still be mutating while ForkSession runs.
func userMessageOrdinalToItemIndex(s *session.Session, ordinal int) (int, error) {
	if ordinal < 0 {
		return 0, fmt.Errorf("%w: %d", ErrForkOutOfRange, ordinal)
	}
	items := s.MessagesSnapshot()
	seen := 0
	for i, item := range items {
		switch {
		case item.IsMessage():
			// Mirror GetAllMessages: system messages don't count.
			if item.Message.Message.Role == chat.MessageRoleSystem {
				continue
			}
			if item.Message.Message.Role != chat.MessageRoleUser {
				continue
			}
			if seen == ordinal {
				return i, nil
			}
			seen++
		case item.IsSubSession():
			subCount := countUserMessages(item.SubSession.GetAllMessages())
			if subCount > 0 && ordinal-seen < subCount {
				return 0, fmt.Errorf("%w at ordinal %d", ErrForkInSubSession, ordinal)
			}
			seen += subCount
		}
	}
	return 0, fmt.Errorf("%w: %d", ErrForkOutOfRange, ordinal)
}

func countUserMessages(msgs []session.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Message.Role == chat.MessageRoleUser {
			n++
		}
	}
	return n
}

// GetSessions retrieves all sessions.
func (sm *SessionManager) GetSessions(ctx context.Context) ([]*session.Session, error) {
	sessions, err := sm.sessionStore.GetSessions(ctx)
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// DeleteSession deletes a session by ID. It cancels the runtime context and
// removes the session from all registries.
func (sm *SessionManager) DeleteSession(ctx context.Context, sessionID string) error {
	if rs, ok := sm.runtimeSessions.Load(sessionID); ok && rs.registry != nil {
		if err := rs.registry.DeleteSession(ctx, sessionID); err != nil {
			return err
		}
		sm.forgetDeletedSessions(ctx, rs.registry, sessionID)
		return nil
	}
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	registry := sm.sessionRegistry
	if source := sess.AttributesSnapshot()[sessionSourceAttribute]; source != "" {
		registry = sm.sessionRegistries[source]
		if registry == nil {
			return &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: sessionID, Operation: "source"}
		}
	}
	if registry != nil {
		if err := registry.DeleteSession(ctx, sessionID); err != nil {
			return err
		}
		sm.forgetDeletedSessions(ctx, registry, sessionID)
		return nil
	}
	if sess.ParentID != "" || sess.AttributesSnapshot()[sessionAgentAttribute] != "" {
		return &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sessionID, Operation: "delete"}
	}
	if err := sm.sessionStore.DeleteSession(ctx, sessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
		return err
	}
	return nil
}

// canonicalSessionDeleted deliberately requires durable absence as well as a
// missing driver: Release is retryable and must not be confused with Delete.
func canonicalSessionDeleted(ctx context.Context, registry runtime.SessionRuntime, store session.Store, id string) bool {
	if store == nil {
		return false
	}
	_, err := registry.SessionByID(id)
	var sessionErr *runtime.SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
		return false
	}
	_, err = store.GetSession(ctx, id)
	return errors.Is(err, session.ErrNotFound)
}

func (sm *SessionManager) forgetDeletedSessions(ctx context.Context, registry runtime.SessionRuntime, id string) {
	sm.runtimeSessions.Delete(id)
	// Successful deletion is final even if the request was canceled just
	// after the durable operation. Finish invalidating its cached descendants.
	ctx = context.WithoutCancel(ctx)
	sm.runtimeSessions.Range(func(id string, active *activeRuntimes) bool {
		if active.registry == registry && canonicalSessionDeleted(ctx, registry, sm.sessionStore, id) {
			sm.runtimeSessions.Delete(id)
		}
		return true
	})
}

// ErrSessionBusy is returned when a session is already processing a request.
var ErrSessionBusy = errors.New("session is already processing a request")

var (
	ErrAgentNotFound          = errors.New("agent source not found")
	ErrAgentSourceUnavailable = errors.New("agent source unavailable")
)

// ToggleToolApproval toggles the legacy blanket tool approval for a
// session. Routed through the safety mode (see [session.Session.ToggleYolo])
// so the two signals cannot disagree, a toggle-off genuinely revokes the
// blanket approval, and an explicit Balanced/Strict choice survives a
// toggle round-trip.
func (sm *SessionManager) ToggleToolApproval(ctx context.Context, sessionID string) error {
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "policy", ToggleToolsApproved: true})
}

func (sm *SessionManager) SetSessionSafetyPolicy(ctx context.Context, sessionID string, policy session.SafetyPolicy) error {
	if !policy.IsValid() {
		return fmt.Errorf("invalid safety_policy: %q", policy)
	}
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "policy", SafetyPolicy: &policy})
}

func (sm *SessionManager) UpdateSessionPermissions(ctx context.Context, sessionID string, perms *session.PermissionsConfig) error {
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "permissions", Permissions: perms})
}

func (sm *SessionManager) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "title", Title: title})
}

// Only unbound historical rows belong to the catalog editor. Canonical rows
// always route through their runtime, even when currently unloaded.
func (sm *SessionManager) editSession(ctx context.Context, id string, edit runtime.SessionEdit) error {
	if handle := sm.loadedSession(id); handle != nil {
		_, err := handle.Edit(ctx, edit)
		return err
	}
	sess, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if sess.ParentID != "" || sess.AttributesSnapshot()[sessionAgentAttribute] != "" {
		handle, err := sm.Handle(ctx, id)
		if err != nil {
			return err
		}
		_, err = handle.Edit(ctx, edit)
		return err
	}
	sm.mux.Lock()
	defer sm.mux.Unlock()
	// Reload under the catalog lock; never modify a borrowed store pointer.
	sess, err = sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if sm.loadedSession(id) != nil || sess.ParentID != "" || sess.AttributesSnapshot()[sessionAgentAttribute] != "" {
		return ErrSessionBusy
	}
	sess = sess.Clone()
	switch edit.Kind {
	case "policy":
		if edit.ToggleToolsApproved {
			sess.ToggleYolo()
		}
		if edit.SafetyPolicy != nil {
			sess.SetSafetyPolicy(*edit.SafetyPolicy)
		}
		if edit.ToolsApproved != nil {
			sess.SetToolsApproved(*edit.ToolsApproved)
		}
	case "permissions":
		sess.SetPermissions(edit.Permissions)
	case "title":
		sess.SetTitle(edit.Title)
	case "message":
		if edit.MessageIndex < 0 {
			_, err := sm.sessionStore.AddMessage(ctx, id, edit.Message)
			return err
		}
		return sm.sessionStore.UpdateMessage(ctx, id, edit.MessageIndex, edit.Message)
	case "summary":
		return sm.sessionStore.AddSummary(ctx, id, *edit.Summary)
	case "tokens":
		return sm.sessionStore.UpdateSessionTokens(ctx, id, edit.InputTokens, edit.OutputTokens, edit.Cost)
	default:
		return &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: id, Operation: "edit"}
	}
	return sm.sessionStore.UpdateSession(ctx, sess)
}

func (sm *SessionManager) sourceLoadError(agentFilename string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, config.ErrSourceFetchFailed) {
		return fmt.Errorf("%w: load %q: %w", ErrAgentSourceUnavailable, agentFilename, err)
	}
	return fmt.Errorf("load %q: %w", agentFilename, err)
}

func (sm *SessionManager) loadTeam(ctx context.Context, agentFilename string, runConfig *config.RuntimeConfig) (*team.Team, error) {
	agentSource, err := sm.resolveSource(agentFilename)
	if err != nil {
		return nil, err
	}

	t, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	if err != nil {
		return nil, sm.sourceLoadError(agentFilename, err)
	}
	return t, nil
}

// LoadAgentConfig loads an agent configuration through the same source
// resolution boundary as agent execution.
func (sm *SessionManager) LoadAgentConfig(ctx context.Context, agentFilename string) (*latest.Config, error) {
	agentSource, err := sm.resolveSource(agentFilename)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(ctx, agentSource)
	if err != nil {
		return nil, sm.sourceLoadError(agentFilename, err)
	}
	return cfg, nil
}

// resolveSource looks up the agent source for agentFilename.
//
// An exact match is always preferred so that distinct variants served side by
// side (e.g. two gordonTag values in the same process) keep their own sources.
// When there is no exact match, it falls back to matching on a stable identity
// that ignores volatile URL query parameters (see config.StableSourceKey). This
// lets a session created under one variant resume under another after the
// server is relaunched with a different tag — the exact key recorded by the
// client no longer exists, but the underlying agent does.
//
// The fallback only fires when it is unambiguous: if several live sources share
// the same stable identity, resolving would be a guess, so it returns the
// not-found error instead of silently picking one.
func (sm *SessionManager) resolveSource(agentFilename string) (config.Source, error) {
	if agentSource, found := sm.Sources[agentFilename]; found {
		return agentSource, nil
	}

	want := config.StableSourceKey(agentFilename)
	var match config.Source
	var matches int
	for key, source := range sm.Sources {
		if config.StableSourceKey(key) == want {
			match = source
			matches++
		}
	}
	if matches == 1 {
		return match, nil
	}

	return nil, fmt.Errorf("%w: agent not found: %s", ErrAgentNotFound, agentFilename)
}

// GetAgentToolCount loads the agent's team and returns the number of
// tools available to the given agent. When agentName is empty, it
// resolves to the team's default agent.
func (sm *SessionManager) GetAgentToolCount(ctx context.Context, agentFilename, agentName string) (int, error) {
	t, err := sm.loadTeam(ctx, agentFilename, sm.runConfig)
	if err != nil {
		return 0, err
	}
	defer func() {
		if stopErr := t.StopToolSets(ctx); stopErr != nil {
			slog.ErrorContext(ctx, "Failed to stop tool sets", "error", stopErr)
		}
	}()

	a, err := t.AgentOrDefault(agentName)
	if err != nil {
		return 0, err
	}

	agentTools, err := a.Tools(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get tools: %w", err)
	}

	return len(agentTools), nil
}

// UpdateMessage updates a message in a session.
//
// Rejected with ErrSessionBusy while the session is starting or running.
func (sm *SessionManager) UpdateMessage(ctx context.Context, sessionID, msgID string, msg *session.Message) error {
	msgPos, err := strconv.ParseInt(msgID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid message ID %q: %w", msgID, err)
	}
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "message", MessageIndex: msgPos, Message: msg})
}

func (sm *SessionManager) AddSummary(ctx context.Context, sessionID string, item session.Item) error {
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "summary", Summary: &item})
}

func (sm *SessionManager) UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	return sm.editSession(ctx, sessionID, runtime.SessionEdit{Kind: "tokens", InputTokens: inputTokens, OutputTokens: outputTokens, Cost: cost})
}

// ErrModelSwitchingNotSupported is returned when the runtime backing a
// session does not support runtime model switching (e.g. when the agent
// was created without a ModelSwitcherConfig).
var ErrModelSwitchingNotSupported = errors.New("model switching not supported by this runtime")

// ErrSessionNotRunning is returned by methods that require an active
// session for the session when none is found. HTTP handlers map this to
// 404 to distinguish from other runtime errors.
var ErrSessionNotRunning = errors.New("session not found or not running")

// BatchDeleteSessions deletes multiple sessions in a single operation.
func (sm *SessionManager) BatchDeleteSessions(ctx context.Context, sessionIDs []string) (int, []string) {
	deleted := 0
	var failed []string
	for _, id := range sessionIDs {
		if err := sm.DeleteSession(ctx, id); err != nil {
			failed = append(failed, id)
		} else {
			deleted++
		}
	}
	return deleted, failed
}

// BatchExportSessions exports multiple sessions as JSON
func (sm *SessionManager) BatchExportSessions(ctx context.Context, sessionIDs []string) (map[string]any, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	export := make(map[string]any)
	export["export_format"] = "json"
	export["timestamp"] = time.Now().Format(time.RFC3339)

	exportedSessions := make([]map[string]any, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sess, err := sm.sessionStore.GetSession(ctx, sessionID)
		if err != nil {
			continue // Skip sessions that can't be retrieved
		}

		inputTokens, outputTokens := sess.Usage()
		sessData := map[string]any{
			"id":             sess.ID,
			"title":          sess.TitleSnapshot(),
			"created_at":     sess.CreatedAt,
			"messages":       sess.GetAllMessages(),
			"input_tokens":   inputTokens,
			"output_tokens":  outputTokens,
			"working_dir":    sess.WorkingDir,
			"tools_approved": sess.ToolsApproved,
		}
		exportedSessions = append(exportedSessions, sessData)
	}

	export["sessions"] = exportedSessions
	export["session_count"] = len(exportedSessions)

	return export, nil
}

// ExportSessionForRecovery exports a single session as JSON for recovery
func (sm *SessionManager) ExportSessionForRecovery(ctx context.Context, sessionID string) (map[string]any, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	inputTokens, outputTokens := sess.Usage()
	export := map[string]any{
		"id":             sess.ID,
		"title":          sess.TitleSnapshot(),
		"created_at":     sess.CreatedAt,
		"messages":       sess.GetAllMessages(),
		"input_tokens":   inputTokens,
		"output_tokens":  outputTokens,
		"working_dir":    sess.WorkingDir,
		"tools_approved": sess.ToolsApproved,
		"permissions":    sess.ClonePermissions(),
	}
	// Recorded errors are session items, not messages, so GetAllMessages
	// drops them. Export them separately: recovery exports feed diagnostics,
	// and a run that died with e.g. a context overflow is invisible without
	// the error that ended it.
	if errs := sess.GetAllErrors(); len(errs) > 0 {
		export["errors"] = errs
	}
	return export, nil
}
