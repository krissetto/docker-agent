package runtime

import (
	"context"
	"errors"
	"net/http"

	"github.com/docker/docker-agent/pkg/api"
)

func (s *remoteSession) WorkspaceSnapshots(ctx context.Context) (api.WorkspaceSnapshots, error) {
	var out api.WorkspaceSnapshots
	if !s.Metadata().Capabilities.Snapshots {
		return out, sessionUnsupported(s.ID(), "workspace_snapshots")
	}
	err := s.runtime.client.sessionJSON(ctx, http.MethodGet, s.endpoint("workspace-snapshots"), nil, &out)
	if err == nil && (out.SessionID != s.ID() || out.Proof == "") {
		err = errors.New("invalid workspace snapshot response identity")
	}
	return out, err
}

func (s *remoteSession) UndoWorkspaceSnapshot(ctx context.Context, proof string) (api.WorkspaceSnapshotResult, error) {
	return s.restoreWorkspaceSnapshot(ctx, "undo", api.WorkspaceSnapshotRequest{ExpectedProof: proof})
}

func (s *remoteSession) ResetWorkspaceSnapshot(ctx context.Context, proof string, keep int) (api.WorkspaceSnapshotResult, error) {
	return s.restoreWorkspaceSnapshot(ctx, "reset", api.WorkspaceSnapshotRequest{ExpectedProof: proof, Keep: keep})
}

func (s *remoteSession) restoreWorkspaceSnapshot(ctx context.Context, operation string, request api.WorkspaceSnapshotRequest) (api.WorkspaceSnapshotResult, error) {
	var out api.WorkspaceSnapshotResult
	if !s.Metadata().Capabilities.Snapshots {
		return out, sessionUnsupported(s.ID(), "workspace_snapshots")
	}
	err := s.runtime.client.sessionJSON(ctx, http.MethodPost, s.endpoint("workspace-snapshots/"+operation), request, &out)
	if err == nil && out.SessionID != s.ID() {
		err = errors.New("invalid workspace snapshot response identity")
	}
	return out, err
}
