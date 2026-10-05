package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestWorkspaceSnapshotBoundHandleNeverUsesLegacyController(t *testing.T) {
	controller := &targetSnapshotController{}
	a := &App{currentState: sessionState{session: session.New(), handle: &projectionSession{id: "bound"}}, snapshotController: controller}
	require.False(t, a.SnapshotsEnabled())
	require.Empty(t, a.ListSnapshots())
	_, err := a.UndoLastSnapshot(t.Context())
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	_, err = a.ResetSnapshot(t.Context(), 0)
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	require.Empty(t, controller.sessions)
	require.Empty(t, controller.workingDirs)
}
