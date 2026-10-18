package tui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestPaneWorkspaceLayoutOrderAndFocusRestoreAcrossLaunch(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() {
		paths.SetDataDir("")
		paths.SetConfigDir("")
	})
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(false), ShowBanner: new(false)}
		return nil
	}))

	workingDir := t.TempDir()
	newApplication := func(ctx context.Context, id string) *app.App {
		sess := session.New(session.WithID(id), session.WithAgentName("root"), session.WithWorkingDir(workingDir))
		return app.New(ctx, nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
	}
	initial := newApplication(t.Context(), "third")
	firstRun := New(t.Context(), nil, initial, workingDir, nil, WithSupervisor(supervisor.New(nil))).(*appModel)

	for _, id := range []string{"first", "second"} {
		application := newApplication(t.Context(), id)
		sess := application.Session()
		require.NoError(t, firstRun.tuiStore.AddTab(t.Context(), id, workingDir))
		_, err := firstRun.supervisor.AddSession(t.Context(), application, sess, workingDir, nil)
		require.NoError(t, err)
		firstRun.createSessionComponents(id, application, sess)
	}
	tabs, active := firstRun.supervisor.GetTabs()
	firstRun.setTabs(tabs, active)
	firstRun.handleReorderTab(messages.ReorderTabMsg{FromIdx: 0, ToIdx: 2})
	_, _ = firstRun.handleSwitchTab("first")
	layout, ok := newSplitLayout("first").Insert("second", "first", splitRight)
	require.True(t, ok)
	layout.root.ratio = 0.63
	settings := chat.SidebarSettings{Collapsed: true, PreferredWidth: 37}
	firstRun.commitPaneWorkspace(layout)
	firstRun.paneSidebarSettings = &settings
	firstRun.applyPaneSidebarSettings()
	_, _ = firstRun.handleSwitchTab("second")
	firstRun.singlePane()
	firstRun.Shutdown()

	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings.RestoreTabs = new(true)
		return nil
	}))
	restoredInitial := newApplication(t.Context(), "third")
	secondSupervisor := supervisor.New(nil)
	restore := New(t.Context(), nil, restoredInitial, workingDir, nil,
		WithSupervisor(secondSupervisor),
		WithSessionRestorer(func(ctx context.Context, id, _ string) (SpawnedSession, error) {
			application := newApplication(ctx, id)
			return SpawnedSession{App: application, Session: application.Session(), Ownership: RuntimeBorrowed}, nil
		}),
	).(*appModel)
	t.Cleanup(restore.cleanupManagedResources)
	_ = restore.init()

	require.Equal(t, []string{"first", "third"}, workspaceIDs(restore), "workspace order survives restart")
	require.Equal(t, "first", restore.paneWorkspaces.active.anchor, "active workspace survives restart")
	require.Equal(t, []string{"second"}, restore.panes.Sessions(), "single-pane view is restored")
	require.Equal(t, []string{"first", "second"}, restore.paneWorkspaces.active.restore.Sessions(), "hidden panes are retained")
	require.Equal(t, "second", restore.paneFocus(), "the active pane selection is restored")
	require.Equal(t, settings, *restore.paneWorkspaces.active.sidebar, "workspace sidebar state survives restart")
	require.NotNil(t, restore.chatPages["first"])
	require.NotNil(t, restore.chatPages["second"])

	restore.Update(messages.SwitchTabMsg{SessionID: "third", Workspace: true})
	require.Equal(t, "third", restore.paneFocus())
	require.Equal(t, []string{"third"}, restore.panes.Sessions())
	restore.Update(messages.SwitchTabMsg{SessionID: "first", Workspace: true})
	require.Equal(t, "second", restore.paneFocus(), "workspace tab selection returns to its remembered pane")
	require.Equal(t, []string{"second"}, restore.panes.Sessions(), "workspace click preserves the saved single view")
	restore.Update(messages.SwitchTabMsg{SessionID: "first"})
	require.Equal(t, "first", restore.paneFocus(), "focusing a hidden pane restores its workspace")
	require.Equal(t, []string{"first", "second"}, restore.panes.Sessions())
	require.InDelta(t, 0.63, restore.panes.root.ratio, 0.001, "hidden split ratios survive restart")
}
