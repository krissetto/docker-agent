package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

type SessionWorkspaceSnapshots interface {
	WorkspaceSnapshots(ctx context.Context) (api.WorkspaceSnapshots, error)
	UndoWorkspaceSnapshot(ctx context.Context, proof string) (api.WorkspaceSnapshotResult, error)
	ResetWorkspaceSnapshot(ctx context.Context, proof string, keep int) (api.WorkspaceSnapshotResult, error)
}

type workspaceSnapshots struct {
	controller builtins.SnapshotController
}

// WithSnapshotController exposes the same controller that captures runtime hooks.
func WithSnapshotController(controller builtins.SnapshotController) Opt {
	return func(r *LocalRuntime) {
		if controller != nil {
			r.snapshots = &workspaceSnapshots{controller: controller}
		}
	}
}

func workspaceSnapshotProof(id string, infos []builtins.SnapshotInfo) string {
	data, _ := json.Marshal(struct {
		ID    string
		Infos []builtins.SnapshotInfo
	}{id, infos})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *sessionHandle) WorkspaceSnapshots(ctx context.Context) (api.WorkspaceSnapshots, error) {
	if err := ctx.Err(); err != nil {
		return api.WorkspaceSnapshots{}, err
	}
	snapshots := h.runtime.snapshots
	if snapshots == nil {
		return api.WorkspaceSnapshots{}, sessionUnsupported(h.ID(), "workspace_snapshots")
	}
	driver, ok := h.runtime.sessionDrivers.Lookup(h.ID())
	if !ok || driver != h.driver {
		return api.WorkspaceSnapshots{}, &SessionError{Kind: SessionErrorStale, SessionID: h.ID(), Operation: "workspace_snapshots"}
	}
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	if h.driver.stopped {
		return api.WorkspaceSnapshots{}, ErrSessionClosed
	}
	infos := snapshots.controller.List(h.ID())
	files := make([]int, len(infos))
	for i, info := range infos {
		files[i] = info.Files
	}
	return api.WorkspaceSnapshots{SessionID: h.ID(), Enabled: snapshots.controller.Enabled(), Files: files, Proof: workspaceSnapshotProof(h.ID(), infos)}, nil
}

func (h *sessionHandle) UndoWorkspaceSnapshot(ctx context.Context, proof string) (api.WorkspaceSnapshotResult, error) {
	return h.restoreWorkspaceSnapshot(ctx, proof, nil)
}

func (h *sessionHandle) ResetWorkspaceSnapshot(ctx context.Context, proof string, keep int) (api.WorkspaceSnapshotResult, error) {
	if keep < 0 {
		return api.WorkspaceSnapshotResult{}, &SessionError{Kind: SessionErrorInvalid, SessionID: h.ID(), Operation: "workspace_snapshots"}
	}
	return h.restoreWorkspaceSnapshot(ctx, proof, &keep)
}

func (h *sessionHandle) restoreWorkspaceSnapshot(ctx context.Context, proof string, keep *int) (api.WorkspaceSnapshotResult, error) {
	if err := ctx.Err(); err != nil {
		return api.WorkspaceSnapshotResult{}, err
	}
	snapshots := h.runtime.snapshots
	if snapshots == nil {
		return api.WorkspaceSnapshotResult{}, sessionUnsupported(h.ID(), "workspace_snapshots")
	}
	// A retired binding must not retain authority over a replacement driver.
	driver, ok := h.runtime.sessionDrivers.Lookup(h.ID())
	if !ok || driver != h.driver {
		return api.WorkspaceSnapshotResult{}, &SessionError{Kind: SessionErrorStale, SessionID: h.ID(), Operation: "workspace_snapshots"}
	}
	var result api.WorkspaceSnapshotResult
	err := h.driver.durableIO(ctx, func() (sessionIOReservation, error) {
		if err := h.driver.admitLocked(SessionOperationSwitchAgent); err != nil {
			return sessionIOReservation{}, err
		}
		cwd := h.driver.sess.WorkingDir
		if cwd == "" {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorInvalid, SessionID: h.ID(), Operation: "workspace_snapshots", Detail: "session has no bound workspace"}
		}
		h.driver.switchReserved = true
		return sessionIOReservation{write: func(ctx context.Context) error {
			if proof == "" || proof != workspaceSnapshotProof(h.ID(), snapshots.controller.List(h.ID())) {
				return &SessionError{Kind: SessionErrorStale, SessionID: h.ID(), Operation: "workspace_snapshots"}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var files int
			var restored bool
			var err error
			if keep == nil {
				files, restored, err = snapshots.controller.UndoLast(ctx, h.ID(), cwd)
			} else {
				files, restored, err = snapshots.controller.Reset(ctx, h.ID(), cwd, *keep)
			}
			result = api.WorkspaceSnapshotResult{SessionID: h.ID(), RestoredFiles: files, Restored: restored}
			return err
		}, commit: func(err error) error {
			h.driver.switchReserved = false
			return err
		}}, nil
	})
	// A canceled caller does not own the worker's result until acknowledgement.
	if err != nil {
		return api.WorkspaceSnapshotResult{}, err
	}
	return result, nil
}
