package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/sessionbrowser"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestNamedWorkspaceLifecycleRetainsOwnersDraftsRatiosAndEmptyClose(t *testing.T) {
	root := splitTestRoot(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.splitPane("second", "profile", splitRight)
	first := root.paneWorkspaces.active
	first.name = "Build 界"
	root.editor.SetValue("draft second")
	page, editor := root.chatPages["second"], root.editor
	layout := root.panes
	count := root.supervisor.Count()
	root.createWorkspace()
	second := root.paneWorkspaces.active
	require.NotEqual(t, first.id, second.id)
	require.True(t, root.workspaceEmpty())
	require.Equal(t, count, root.supervisor.Count(), "empty workspace allocates no execution owner")
	require.Same(t, page, root.chatPages["second"])
	require.Same(t, editor, root.editors["second"])
	_, cmd := root.Update(messages.SendMsg{Content: "must not reach previous owner"})
	require.NotNil(t, cmd)
	require.Equal(t, "draft second", root.editors["second"].Value())
	root.activateWorkspace(first.id)
	require.Equal(t, layout, root.panes)
	require.Equal(t, "draft second", root.editor.Value())
	root.closeWorkspace(first.id)
	require.Equal(t, second.id, root.paneWorkspaces.active.id)
	require.Equal(t, count, root.supervisor.Count(), "closing membership never closes canonical session")
	require.Same(t, page, root.chatPages["second"])
	root.closeWorkspace(second.id)
	require.Len(t, root.paneWorkspaces.ordered, 1)
	require.True(t, root.workspaceEmpty())
	require.Equal(t, count, root.supervisor.Count())
}

func TestBrowserOpeningLoadedSessionMovesMembershipWithoutDuplicates(t *testing.T) {
	root := splitTestRoot(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.splitPane("second", "profile", splitRight)
	old := root.paneWorkspaces.active
	root.createWorkspace()
	destination := root.paneWorkspaces.active
	root.openWorkspaceSession(root.persistedSessionID("second"), "open")
	require.Same(t, destination, root.paneWorkspaces.active)
	require.Equal(t, []string{"second"}, root.panes.Sessions())
	require.Equal(t, []string{"profile"}, old.layout.Sessions())
	root.openWorkspaceSession(root.persistedSessionID("profile"), "split")
	require.Len(t, root.panes.Sessions(), 2)
	require.Nil(t, old.layout.root)
	requireWorkspaceMembership(t, root)
	root.activateWorkspace(old.id)
	root.openWorkspaceSession(root.persistedSessionID("third"), "open")
	require.Equal(t, []string{"third"}, root.panes.Sessions())
	root.openWorkspaceSession(root.persistedSessionID("second"), "open")
	require.Equal(t, []string{"second"}, root.panes.Sessions())
	require.Nil(t, root.paneWorkspaces.byRoute["third"], "displaced route is detached but remains open")
	require.NotNil(t, root.supervisor.GetRunner("third"))
}

func TestWorkspaceBrowserGeometryNarrowAndInputRouting(t *testing.T) {
	root := splitTestRoot(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.workspaceUI.visible = true
	root.resizeAll()
	require.False(t, root.workspaceUI.fullscreen)
	require.GreaterOrEqual(t, root.paneBounds.X, root.workspaceUI.browserWidth)
	root.handleWindowResize(50, 25)
	require.True(t, root.workspaceUI.fullscreen)
	root.workspaceUI.browser.Focus()
	root.handleKeyPress(tea.KeyPressMsg{Code: '/', Text: "/"})
	root.Update(tea.PasteMsg{Content: "Search title 世界"})
	require.Equal(t, "Search title 世界", root.workspaceUI.browser.Query())
	require.NotEqual(t, "Search title 世界", root.editor.Value())
	root.handleWindowResize(140, 40)
	require.False(t, root.workspaceUI.fullscreen)
	require.GreaterOrEqual(t, root.paneBounds.X, root.workspaceUI.browserWidth)
}

func TestWorkspacePersistenceCanonicalIDsMissingTopologyAndOptOut(t *testing.T) {
	root := splitTestRoot(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.splitPane("second", "profile", splitRight)
	root.panes.root.ratio = 0.37
	root.savePaneWorkspace(root.paneFocus())
	first := root.paneWorkspaces.active
	first.name = "Build saved"
	state := root.workspaceState()
	require.NoError(t, state.Validate())
	require.Equal(t, 0.37, state.Workspaces[0].Layout.Ratio)
	// A saved missing route remains represented until explicit removal, not silently pruned.
	saved := &tuistate.LayoutNode{Axis: tuistate.SplitColumns, Ratio: 0.23, First: &tuistate.LayoutNode{SessionID: root.persistedSessionID("profile")}, Second: &tuistate.LayoutNode{SessionID: "missing-session"}}
	first.saved = saved
	first.unavailable = make(map[string]string)
	first.layout = root.restoreWorkspaceLayout(saved, first)
	root.panes = first.layout
	state = root.workspaceState()
	require.NoError(t, state.Validate())
	require.Equal(t, "missing-session", state.Workspaces[0].Layout.Second.SessionID)
	require.Equal(t, 0.23, state.Workspaces[0].Layout.Ratio)
	require.Contains(t, first.unavailable, "missing-session")
	root.removeMissingSession("missing-session")
	state = root.workspaceState()
	require.NoError(t, state.Validate())
	require.Equal(t, root.persistedSessionID("profile"), state.Workspaces[0].Layout.SessionID)
}

func TestWorkspaceDefinitionsReloadWithoutAutoMountWhenRestoreDisabled(t *testing.T) {
	root := splitTestRoot(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		if cfg.Settings == nil {
			cfg.Settings = &userconfig.Settings{}
		}
		cfg.Settings.RestoreTabs = new(false)
		return nil
	}))
	state := tuistate.WorkspaceState{Version: 1, ActiveID: "saved", Workspaces: []tuistate.Workspace{{ID: "saved", Name: "Retained", Layout: &tuistate.LayoutNode{SessionID: "unmounted-session"}, FocusedSessionID: "unmounted-session"}}}
	require.NoError(t, root.tuiStore.SaveWorkspaces(t.Context(), state))
	count := root.supervisor.Count()
	root.initializeWorkspaces()
	require.Equal(t, count, root.supervisor.Count())
	require.Equal(t, "Retained", root.paneWorkspaces.ordered[0].name)
	require.Nil(t, root.paneWorkspaces.ordered[0].layout.root)
	require.Contains(t, root.paneWorkspaces.ordered[0].unavailable, "unmounted-session")
	require.NotEqual(t, "saved", root.paneWorkspaces.active.id)
	require.NoError(t, root.persistWorkspaces())
	persisted, err := root.tuiStore.GetWorkspaces(t.Context())
	require.NoError(t, err)
	require.Equal(t, "unmounted-session", persisted.Workspaces[0].Layout.SessionID)
}

func TestUnmountedWorkspaceRetainsDesiredCanonicalFocusAndRatios(t *testing.T) {
	root := splitTestRoot(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.savePaneWorkspace(root.paneFocus())
	w := root.paneWorkspaces.active
	w.saved = &tuistate.LayoutNode{Axis: tuistate.SplitColumns, Ratio: 0.31, First: &tuistate.LayoutNode{SessionID: "missing-left"}, Second: &tuistate.LayoutNode{SessionID: "missing-focused"}}
	w.desiredFocus = "missing-focused"
	w.layout = splitLayout{}
	w.focus = ""
	root.panes = w.layout
	root.markWorkspaceUnmounted(w.saved, w)
	state := root.workspaceState()
	require.NoError(t, state.Validate())
	require.Equal(t, "missing-focused", state.Workspaces[0].FocusedSessionID)
	require.Equal(t, 0.31, state.Workspaces[0].Layout.Ratio)
	// Canonical intent survives repeated persistence while no route is mounted.
	require.Equal(t, state, root.workspaceState())
	root.removeMissingSession("missing-left")
	state = root.workspaceState()
	require.NoError(t, state.Validate())
	require.Equal(t, "missing-focused", state.Workspaces[0].FocusedSessionID)
	root.removeMissingSession("missing-focused")
	state = root.workspaceState()
	require.NoError(t, state.Validate())
	require.Empty(t, state.Workspaces[0].FocusedSessionID)
}
