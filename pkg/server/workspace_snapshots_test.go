package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type workspaceSnapshotStub struct {
	history map[string][]builtins.SnapshotInfo
	cwd     string
	calls   int
}

func (*workspaceSnapshotStub) Enabled() bool            { return true }
func (*workspaceSnapshotStub) AutoInject(*hooks.Config) {}
func (s *workspaceSnapshotStub) List(id string) []builtins.SnapshotInfo {
	return append([]builtins.SnapshotInfo(nil), s.history[id]...)
}

func (s *workspaceSnapshotStub) UndoLast(ctx context.Context, id, cwd string) (int, bool, error) {
	return s.Reset(ctx, id, cwd, len(s.history[id])-1)
}

func (s *workspaceSnapshotStub) Reset(ctx context.Context, id, cwd string, keep int) (int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s.cwd = cwd
	s.calls++
	if keep < 0 || keep >= len(s.history[id]) {
		return 0, false, nil
	}
	count := 0
	for _, item := range s.history[id][keep:] {
		count += item.Files
	}
	s.history[id] = s.history[id][:keep]
	return count, true, nil
}

func TestWorkspaceSnapshotHTTPParityAndIdentity(t *testing.T) {
	controller := &workspaceSnapshotStub{history: map[string][]builtins.SnapshotInfo{}}
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithSnapshotController(controller))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	server := NewWithManager(sm, "")
	httpServer := httptest.NewServer(server.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	workspace := t.TempDir()
	handle, err := transport.CreateSession(t.Context(), session.New(session.WithWorkingDir(workspace)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	id := handle.ID()
	controller.history[id] = []builtins.SnapshotInfo{{ID: 1, Files: 2}, {ID: 2, Files: 3}}
	remote := handle.(runtime.SessionWorkspaceSnapshots)
	history, err := remote.WorkspaceSnapshots(t.Context())
	require.NoError(t, err)
	require.Equal(t, []int{2, 3}, history.Files)
	canonical, err := owner.Runtime().SessionByID(id)
	require.NoError(t, err)
	localHistory, err := canonical.(runtime.SessionWorkspaceSnapshots).WorkspaceSnapshots(t.Context())
	require.NoError(t, err)
	require.Equal(t, history, localHistory)
	result, err := remote.UndoWorkspaceSnapshot(t.Context(), history.Proof)
	require.NoError(t, err)
	require.Equal(t, 3, result.RestoredFiles)
	require.Equal(t, workspace, controller.cwd)
	_, err = remote.UndoWorkspaceSnapshot(t.Context(), history.Proof)
	require.Error(t, err)
	require.Equal(t, 1, controller.calls)
	// Reconnection observes the controller, not state cached in the old viewer.
	reconnect, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	replacement, err := reconnect.SessionByID(id)
	require.NoError(t, err)
	_, err = replacement.Status(t.Context())
	require.NoError(t, err)
	reconnected := replacement.(runtime.SessionWorkspaceSnapshots)
	history, err = reconnected.WorkspaceSnapshots(t.Context())
	require.NoError(t, err)
	require.Equal(t, []int{2}, history.Files)
	other, err := transport.CreateSession(t.Context(), session.New(session.WithWorkingDir(t.TempDir())), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	_, err = other.(runtime.SessionWorkspaceSnapshots).ResetWorkspaceSnapshot(t.Context(), history.Proof, 0)
	require.Error(t, err)
	require.Equal(t, 1, controller.calls)
	result, err = reconnected.ResetWorkspaceSnapshot(t.Context(), history.Proof, 0)
	require.NoError(t, err)
	require.Equal(t, 2, result.RestoredFiles)
	history, err = reconnected.WorkspaceSnapshots(t.Context())
	require.NoError(t, err)
	require.Empty(t, history.Files)
	rec := sessionRequest(t, server, http.MethodPost, api.SessionAPIPath+"/"+id+"/workspace-snapshots/reset", `{"expected_proof":"`+history.Proof+`","cwd":"/foreign"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.True(t, handle.Metadata().Capabilities.Snapshots)
}
