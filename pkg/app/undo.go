package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

var ErrNothingToUndo = errors.New("nothing to undo")

type UndoSnapshotResult struct {
	RestoredFiles int
}

// SnapshotsEnabled reports the bound session capability, or the legacy
// controller capability when no canonical handle is attached.
func (a *App) SnapshotsEnabled() bool {
	state := a.state()
	if state.handle != nil {
		return state.handle.Metadata().Capabilities.Snapshots
	}
	return a.snapshotController != nil && a.snapshotController.Enabled()
}

// UndoLastSnapshot restores the files captured in the most recent
// snapshot checkpoint for the current session.
func (a *App) UndoLastSnapshot(ctx context.Context) (UndoSnapshotResult, error) {
	state := a.state()
	if capability, ok := state.handle.(runtime.SessionWorkspaceSnapshots); ok && state.handle.Metadata().Capabilities.Snapshots {
		history, err := capability.WorkspaceSnapshots(ctx)
		if err != nil {
			return UndoSnapshotResult{}, err
		}
		result, err := capability.UndoWorkspaceSnapshot(ctx, history.Proof)
		return snapshotResult(result.RestoredFiles, result.Restored, err)
	}
	if state.handle != nil {
		return UndoSnapshotResult{}, runtime.UnsupportedSessionOperation(state.handle.ID(), "workspace_snapshots")
	}
	if a.snapshotController == nil || state.session == nil {
		return UndoSnapshotResult{}, ErrNothingToUndo
	}
	return snapshotResult(a.snapshotController.UndoLast(ctx, state.session.ID, snapshotCwd(state.session)))
}

// ListSnapshots returns the file count of every snapshot captured during
// the current session, oldest first. Returns nil when no snapshots exist
// or when no controller is configured.
func (a *App) ListSnapshots() []int {
	state := a.state()
	if capability, ok := state.handle.(runtime.SessionWorkspaceSnapshots); ok && state.handle.Metadata().Capabilities.Snapshots {
		history, err := capability.WorkspaceSnapshots(a.ctx())
		if err != nil {
			return nil
		}
		return history.Files
	}
	if state.handle != nil {
		return nil
	}
	if a.snapshotController == nil || state.session == nil {
		return nil
	}
	infos := a.snapshotController.List(state.session.ID)
	counts := make([]int, len(infos))
	for i, info := range infos {
		counts[i] = info.Files
	}
	return counts
}

// ResetSnapshot reverts every checkpoint past index keep so the workspace
// returns to the state captured at that snapshot. keep == 0 resets to
// the original pre-agent state.
func (a *App) ResetSnapshot(ctx context.Context, keep int) (UndoSnapshotResult, error) {
	state := a.state()
	if capability, ok := state.handle.(runtime.SessionWorkspaceSnapshots); ok && state.handle.Metadata().Capabilities.Snapshots {
		history, err := capability.WorkspaceSnapshots(ctx)
		if err != nil {
			return UndoSnapshotResult{}, err
		}
		result, err := capability.ResetWorkspaceSnapshot(ctx, history.Proof, keep)
		return snapshotResult(result.RestoredFiles, result.Restored, err)
	}
	if state.handle != nil {
		return UndoSnapshotResult{}, runtime.UnsupportedSessionOperation(state.handle.ID(), "workspace_snapshots")
	}
	if a.snapshotController == nil || state.session == nil {
		return UndoSnapshotResult{}, ErrNothingToUndo
	}
	return snapshotResult(a.snapshotController.Reset(ctx, state.session.ID, snapshotCwd(state.session), keep))
}

// snapshotCwd resolves the working directory the snapshot operations
// should run against. Sessions carry their own WorkingDir (set by the
// embedder when the session is constructed); if it's empty we fall
// back to os.Getwd so snapshot commands keep working in setups that
// don't propagate a working dir on the session.
func snapshotCwd(sess *session.Session) string {
	if sess != nil && sess.WorkingDir != "" {
		return sess.WorkingDir
	}
	cwd, _ := os.Getwd()
	return cwd
}

// snapshotResult adapts the (files, ok, err) tuple returned by snapshot
// operations into the UndoSnapshotResult / ErrNothingToUndo shape callers
// expect.
func snapshotResult(files int, ok bool, err error) (UndoSnapshotResult, error) {
	if err != nil {
		return UndoSnapshotResult{}, fmt.Errorf("restoring snapshot: %w", err)
	}
	if !ok {
		return UndoSnapshotResult{}, ErrNothingToUndo
	}
	return UndoSnapshotResult{RestoredFiles: files}, nil
}
