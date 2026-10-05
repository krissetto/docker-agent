package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/stretchr/testify/require"
)

func TestParityRemoteWorkspaceNeverUsesClientFilesystem(t *testing.T) {
	for _, workspace := range []string{"/server/does-not-exist-on-client", ""} {
		spy := &spySpawner{}
		m := newSpawnTestModel(t, spy, WithRemoteWorkspace(workspace))
		dir, err := m.resolveNewSessionDir(".")
		require.NoError(t, err)
		require.Equal(t, workspace, dir)
		dir, err = m.resolveNewSessionDir("/another/server-only/path")
		require.NoError(t, err)
		require.Equal(t, "/another/server-only/path", dir)
		dir, err = m.resolveNewSessionDir("relative/server-path")
		require.NoError(t, err)
		require.Equal(t, "relative/server-path", dir)
		m.handleSpawnSession("")
		require.Equal(t, []string{workspace}, spy.dirs)
	}
}

func TestParityAsyncResultRejectedAfterSessionEpoch(t *testing.T) {
	root := panelFixture(t)
	original := root.editor.Value()
	cmd := root.capabilityCommand(func(context.Context, *app.App) tea.Msg {
		return messages.RestorePendingMessagesMsg{Content: "remote prompt"}
	})
	root.modelPickerGeneration++
	result := cmd().(capabilityResult)
	require.False(t, root.capabilityScopeCurrent(result.scope))
	root.update(result)
	require.Equal(t, original, root.editor.Value())
	cmd = root.capabilityCommand(func(context.Context, *app.App) tea.Msg {
		return messages.RestorePendingMessagesMsg{Content: "remote prompt"}
	})
	root.update(cmd())
	require.Equal(t, "remote prompt", root.editor.Value())
}

func TestParityRestorerDoesNotCreateBlankSessionsOrReadLocalStore(t *testing.T) {
	setupSettingsConfigTest(t)
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
	enabled := true
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: &enabled}
		return nil
	}))
	ts, err := tuistate.New(t.Context())
	require.NoError(t, err)
	defer ts.Close()
	require.NoError(t, ts.AddTab(t.Context(), "saved", "/server/workspace"))
	require.NoError(t, ts.SetActiveTab(t.Context(), "saved"))
	spy := &spySpawner{}
	root := newSpawnTestModel(t, spy)
	// Empty local store would discard the saved row under the old startup logic.
	local := session.NewInMemorySessionStore()
	initial := root.application.Session()
	root.application = app.New(t.Context(), nil, initial, runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: local}))
	var restored []string
	WithSessionRestorer(func(ctx context.Context, id, dir string) (SpawnedSession, error) {
		restored = append(restored, id)
		sess := session.New(session.WithID(id), session.WithWorkingDir(dir))
		return SpawnedSession{App: app.New(ctx, nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), Session: sess, Ownership: RuntimeBorrowed}, nil
	})(root)
	root.restoreTabs(t.Context(), ts, root.supervisor, spy.spawn, root.application, initial.ID, initial.WorkingDir)
	require.Equal(t, []string{"saved"}, restored)
	require.Empty(t, spy.dirs, "restoration cannot issue create through spawner")
	require.Empty(t, root.pendingRestores, "restored identity is attached, not a placeholder")
	require.NotNil(t, root.supervisor.FindBySession("saved"))
	root.supervisor.Shutdown()
}

func TestParityMetadataSurvivesModelPickerGeneration(t *testing.T) {
	root := panelFixture(t)
	root.metadataApp = nil
	command := root.prepareCommandMetadata()
	require.NotNil(t, command)
	root.modelPickerGeneration++ // opening a model picker while metadata is loading
	result := command().(capabilityResult)
	require.Equal(t, root.metadataGeneration, result.metadataGeneration)
	calls := 0
	root.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { calls++; return nil }
	_, cmd := root.update(result)
	// A stale picker epoch previously discarded this refresh altogether.
	require.Equal(t, 1, calls)
	old := result
	root.metadataApp = nil
	next := root.prepareCommandMetadata()
	require.NotNil(t, next)
	_, cmd = root.update(old)
	require.Nil(t, cmd, "replaced metadata request cannot publish")
}
