package builtins

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/snapshot"
)

// Snapshot is the registered name of the snapshot builtin.
const Snapshot = "snapshot"

// SnapshotInfo summarises one completed snapshot checkpoint for display.
type SnapshotInfo struct {
	ID uint64
	// Files is the number of unique files captured in the checkpoint.
	Files int
}

// SnapshotController exposes the operations the embedder uses to drive
// shadow-git snapshot commands (/undo, /snapshots, /reset). It is
// returned by [RegisterSnapshot]. The runtime exposes its operations through
// canonical session handles; legacy embedders may also use it directly.
//
// Enabled() reports whether snapshot auto-injection (capturing
// checkpoints at session/turn boundaries) is configured. The other
// methods always work against any checkpoints already captured for
// sessionID, regardless of Enabled().
//
// SnapshotController also satisfies [AutoInjector] so the same
// instance the App uses for /undo can be passed to the runtime via
// runtime.WithAutoInjector. AutoInject is a runtime-internal call;
// embedders normally don't invoke it directly.
type SnapshotController interface {
	AutoInjector
	Enabled() bool
	UndoLast(ctx context.Context, sessionID, cwd string) (files int, ok bool, err error)
	List(sessionID string) []SnapshotInfo
	Reset(ctx context.Context, sessionID, cwd string, keep int) (files int, ok bool, err error)
}

// RegisterSnapshot installs the snapshot builtin on r and returns a
// [SnapshotController]. enabled controls whether the controller's
// AutoInject mounts the snapshot hook on session/turn boundaries; pass
// false to keep the hook resolvable for users who wire it manually via
// YAML without auto-capturing checkpoints.
//
// Pass the same controller to runtime.WithAutoInjector and
// runtime.WithSnapshotController so canonical handles restore the checkpoints
// captured by hooks. Legacy embedders may use app.WithSnapshotController.
func RegisterSnapshot(r *hooks.Registry, enabled bool) (SnapshotController, error) {
	b := newSnapshotBuiltin()
	if err := r.RegisterBuiltin(Snapshot, b.hook); err != nil {
		return nil, err
	}
	return &snapshotController{builtin: b, enabled: enabled}, nil
}

type snapshotController struct {
	builtin *snapshotBuiltin
	enabled bool
}

var (
	_ SnapshotController = (*snapshotController)(nil)
	_ AutoInjector       = (*snapshotController)(nil)
)

func (c *snapshotController) Enabled() bool {
	return c != nil && c.enabled
}

func (c *snapshotController) UndoLast(ctx context.Context, sessionID, cwd string) (int, bool, error) {
	if c == nil || c.builtin == nil || sessionID == "" || cwd == "" {
		return 0, false, nil
	}
	return c.builtin.undoLast(ctx, sessionID, cwd)
}

func (c *snapshotController) List(sessionID string) []SnapshotInfo {
	if c == nil || c.builtin == nil || sessionID == "" {
		return nil
	}
	return c.builtin.listSnapshots(sessionID)
}

func (c *snapshotController) Reset(ctx context.Context, sessionID, cwd string, keep int) (int, bool, error) {
	if c == nil || c.builtin == nil || sessionID == "" || cwd == "" {
		return 0, false, nil
	}
	return c.builtin.resetSnapshot(ctx, sessionID, cwd, keep)
}

// AutoInject mounts the snapshot hook on session/turn boundaries when
// the controller is enabled. The four-event surface (session_start,
// turn_start, turn_end, session_end) matches what the snapshot builtin
// needs to bracket every session and every model turn; per-tool
// capture (pre_tool_use / post_tool_use) is opt-in via YAML and is not
// auto-wired here.
func (c *snapshotController) AutoInject(cfg *hooks.Config) {
	if c == nil || !c.enabled || cfg == nil {
		return
	}
	hook := hooks.Hook{Type: hooks.HookTypeBuiltin, Command: Snapshot}
	cfg.SessionStart = append(cfg.SessionStart, hook)
	cfg.TurnStart = append(cfg.TurnStart, hook)
	cfg.TurnEnd = append(cfg.TurnEnd, hook)
	cfg.SessionEnd = append(cfg.SessionEnd, hook)
}

// snapshotBuiltin tracks per-session shadow-git checkpoints. The same
// instance is dispatched as the snapshot builtin (registered under
// [Snapshot] via [snapshotBuiltin.hook]) and exposed to embedders via
// [snapshotController] for /undo, /snapshots, and /reset. Construct
// with [newSnapshotBuiltin]; the zero value is not usable.
type snapshotBuiltin struct {
	manager        *snapshot.Manager
	mu             sync.Mutex
	session        map[string]*snapshotSession
	nextCheckpoint uint64
}

type snapshotSession struct {
	restoreMu sync.Mutex
	turn      string
	tools     map[string]string
	history   []snapshotCheckpoint
}

type snapshotCheckpoint struct {
	id    uint64
	hash  string
	files []string
}

// newSnapshotBuiltin returns a fresh snapshot tracker. Held by
// [snapshotController] for /undo, /snapshots and /reset; the same
// instance backs the snapshot hook registered under [Snapshot].
func newSnapshotBuiltin() *snapshotBuiltin {
	return &snapshotBuiltin{
		manager: snapshot.NewManager(""),
		session: map[string]*snapshotSession{},
	}
}

// hook is the [hooks.BuiltinFunc] dispatched on every snapshot event.
// It tracks per-session turn/tool hashes, captures patches at
// turn_end / post_tool_use, and runs the shadow-repo cleanup at
// session_end.
func (b *snapshotBuiltin) hook(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" || in.SessionID == "" {
		return nil, nil
	}
	repo, err := b.manager.Open(ctx, in.Cwd)
	if err != nil {
		if errors.Is(err, snapshot.ErrNotGitRepository) {
			return nil, nil
		}
		slog.DebugContext(ctx, "snapshot hook: open repository failed; skipping", "cwd", in.Cwd, "error", err)
		return nil, nil
	}

	track := func() (string, bool) {
		hash, err := repo.Track(ctx)
		if err != nil {
			slog.DebugContext(ctx, "snapshot hook: track failed", "cwd", in.Cwd, "error", err)
			return "", false
		}
		return hash, true
	}
	patchFrom := func(hash string) (snapshot.Patch, string, bool) {
		after, ok := track()
		if !ok {
			return snapshot.Patch{}, "", false
		}
		patch, err := repo.Patch(ctx, hash)
		if err != nil {
			slog.DebugContext(ctx, "snapshot hook: patch failed", "cwd", in.Cwd, "hash", hash, "error", err)
			return snapshot.Patch{}, "", false
		}
		return patch, after, true
	}

	switch in.HookEventName {
	case hooks.EventSessionStart:
		track()
	case hooks.EventTurnStart:
		if hash, ok := track(); ok {
			b.setTurn(in.SessionID, hash)
		}
	case hooks.EventPreToolUse:
		if hash, ok := track(); ok {
			b.setTool(in.SessionID, in.ToolUseID, hash)
		}
	case hooks.EventPostToolUse:
		hash := b.popTool(in.SessionID, in.ToolUseID)
		if hash == "" {
			return nil, nil
		}
		if patch, after, ok := patchFrom(hash); ok {
			b.pushCheckpoint(in.SessionID, snapshotCheckpoint{hash: hash, files: patch.Files})
			logPatch(ctx, "tool", in.SessionID, in.ToolName, patch, after)
		}
	case hooks.EventTurnEnd:
		hash := b.popTurn(in.SessionID)
		if hash == "" {
			return nil, nil
		}
		if patch, after, ok := patchFrom(hash); ok {
			b.pushCheckpoint(in.SessionID, snapshotCheckpoint{hash: hash, files: patch.Files})
			logPatch(ctx, "turn", in.SessionID, in.Reason, patch, after)
		}
	case hooks.EventSessionEnd:
		if err := repo.Cleanup(ctx); err != nil {
			slog.DebugContext(ctx, "snapshot hook: cleanup failed", "cwd", in.Cwd, "error", err)
		}
	default:
		slog.DebugContext(ctx, "snapshot hook configured on unsupported event; skipping", "event", in.HookEventName)
	}
	return nil, nil
}

func (b *snapshotBuiltin) getSession(sessionID string) *snapshotSession {
	s := b.session[sessionID]
	if s == nil {
		s = &snapshotSession{tools: map[string]string{}}
		b.session[sessionID] = s
	}
	return s
}

func (b *snapshotBuiltin) setTurn(sessionID, hash string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getSession(sessionID).turn = hash
}

func (b *snapshotBuiltin) popTurn(sessionID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session[sessionID]
	if s == nil {
		return ""
	}
	hash := s.turn
	s.turn = ""
	return hash
}

func (b *snapshotBuiltin) setTool(sessionID, toolUseID, hash string) {
	if toolUseID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getSession(sessionID).tools[toolUseID] = hash
}

func (b *snapshotBuiltin) popTool(sessionID, toolUseID string) string {
	if toolUseID == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session[sessionID]
	if s == nil {
		return ""
	}
	hash := s.tools[toolUseID]
	delete(s.tools, toolUseID)
	return hash
}

func (b *snapshotBuiltin) pushCheckpoint(sessionID string, checkpoint snapshotCheckpoint) {
	if len(checkpoint.files) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.getSession(sessionID)
	b.nextCheckpoint++
	checkpoint.id = b.nextCheckpoint
	s.history = append(s.history, checkpoint)
}

// undoLast restores the files captured by the most recent checkpoint.
// Returns (filesRestored, true, nil) on success, (0, false, nil) when
// there is nothing to undo.
func (b *snapshotBuiltin) undoLast(ctx context.Context, sessionID, cwd string) (files int, ok bool, err error) {
	b.mu.Lock()
	s := b.getSession(sessionID)
	b.mu.Unlock()
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	b.mu.Lock()
	if len(s.history) == 0 {
		b.mu.Unlock()
		return 0, false, nil
	}
	checkpoint := s.history[len(s.history)-1]
	b.mu.Unlock()
	repo, err := b.manager.Open(ctx, cwd)
	if err != nil {
		return 0, true, err
	}
	patch := snapshot.Patch{Hash: checkpoint.hash, Files: checkpoint.files}
	if err := repo.Revert(ctx, []snapshot.Patch{patch}); err != nil {
		return 0, true, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, entry := range s.history {
		if entry.id == checkpoint.id {
			s.history = append(s.history[:i], s.history[i+1:]...)
			break
		}
	}
	return len(checkpoint.files), true, nil
}

// listSnapshots returns the completed checkpoints for a session in
// chronological order (oldest first). The returned slice may be empty.
func (b *snapshotBuiltin) listSnapshots(sessionID string) []SnapshotInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.session[sessionID]
	if s == nil {
		return nil
	}
	out := make([]SnapshotInfo, len(s.history))
	for i, c := range s.history {
		out[i] = SnapshotInfo{ID: c.id, Files: len(c.files)}
	}
	return out
}

// resetSnapshot reverts every checkpoint with index >= keep so the
// workspace returns to the state captured at snapshot keep. keep == 0
// means "reset to the original state". A keep value greater than or
// equal to the snapshot count is a no-op. Reverted checkpoints are
// dropped from the session history.
func (b *snapshotBuiltin) resetSnapshot(ctx context.Context, sessionID, cwd string, keep int) (files int, ok bool, err error) {
	b.mu.Lock()
	s := b.getSession(sessionID)
	b.mu.Unlock()
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	b.mu.Lock()
	if keep < 0 {
		keep = 0
	}
	if keep >= len(s.history) {
		b.mu.Unlock()
		return 0, false, nil
	}
	tail := append([]snapshotCheckpoint(nil), s.history[keep:]...)
	b.mu.Unlock()
	repo, err := b.manager.Open(ctx, cwd)
	if err != nil {
		return 0, true, err
	}
	patches := make([]snapshot.Patch, len(tail))
	seen := map[string]bool{}
	for i, c := range tail {
		patches[i] = snapshot.Patch{Hash: c.hash, Files: c.files}
		for _, f := range c.files {
			seen[f] = true
		}
	}
	if err := repo.Revert(ctx, patches); err != nil {
		return 0, true, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Hooks may append while a legacy embedder restores; retain those checkpoints.
	copy(s.history[keep:], s.history[keep+len(tail):])
	clear(s.history[len(s.history)-len(tail):])
	s.history = s.history[:len(s.history)-len(tail)]
	return len(seen), true, nil
}

func logPatch(ctx context.Context, scope, sessionID, label string, patch snapshot.Patch, after string) {
	if len(patch.Files) == 0 {
		return
	}
	slog.DebugContext(ctx, "snapshot captured changes",
		"scope", scope,
		"session_id", sessionID,
		"label", label,
		"hash", patch.Hash,
		"after", after,
		"files", len(patch.Files),
	)
}
