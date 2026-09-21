package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func requireWorkspaceMembership(t *testing.T, root *appModel) {
	t.Helper()
	seen := make(map[*paneWorkspace]bool)
	for id, owner := range root.paneWorkspaces.byRoute {
		require.True(t, owner.layout.Contains(id), "route %s missing from its workspace", id)
		if seen[owner] {
			continue
		}
		seen[owner] = true
		for _, route := range owner.layout.Sessions() {
			require.Same(t, owner, root.paneWorkspaces.byRoute[route], "duplicate route %s", route)
		}
	}
	if root.paneWorkspaces.active != nil {
		require.Same(t, root.panes.root, root.paneWorkspaces.active.layout.root)
	}
}

func TestPaneWorkspaceNewTabRestoresSplitRatiosDraftsAndSidebar(t *testing.T) {
	root := splitTestRoot(t)
	root.editor.SetValue("first draft λ")
	root.splitPane("second", "profile", splitRight)
	root.editor.SetValue("second draft 界")
	d := root.paneGeometry.Dividers[0]
	root.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: d.ID})
	root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	settings := chat.SidebarSettings{Collapsed: true, PreferredWidth: 37}
	root.paneSidebarSettings = &settings
	root.applyPaneSidebarSettings()
	saved := root.panes
	pages, editors := root.chatPages["second"], root.editors["second"]

	// The third route is unassigned, just like a freshly spawned tab.
	root.handleSwitchTab("third")
	require.Equal(t, []string{"third"}, root.panes.Sessions())
	require.Len(t, root.paneGeometry.Panes, 1)
	root.editor.SetValue("third draft")
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.StreamStarted("profile", "root")})
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.AgentChoice("root", "profile", "workspace background update")})
	root.handleSwitchTab("second")
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, saved, root.panes)
	require.Equal(t, settings, *root.paneSidebarSettings)
	require.Equal(t, "second draft 界", root.editor.Value())
	require.Same(t, pages, root.chatPages["second"])
	require.Same(t, editors, root.editors["second"])
	require.Contains(t, root.View().Content, "workspace background update")
	root.handleSwitchTab("profile")
	require.Equal(t, "first draft λ", root.editor.Value())
	root.handleSwitchTab("third")
	require.Equal(t, "third draft", root.editor.Value())
	requireWorkspaceMembership(t, root)
}

func TestPaneWorkspaceSpawnOpensFullscreenWithoutReplacingSplit(t *testing.T) {
	root := newSpawnTestModel(t, &spySpawner{})
	t.Cleanup(root.cleanupManagedResources)
	root.handleSpawnSession(t.TempDir())
	first := root.paneFocus()
	root.handleSpawnSession(t.TempDir())
	second := root.paneFocus()
	root.handleWindowResize(120, 40)
	root.splitPane(first, second, splitRight)
	require.Len(t, root.panes.Sessions(), 2)
	saved := root.panes
	root.handleSpawnSession(t.TempDir())
	third := root.paneFocus()
	require.Equal(t, []string{third}, root.panes.Sessions())
	require.Len(t, root.paneGeometry.Panes, 1)
	root.handleSwitchTab(second)
	require.Equal(t, saved, root.panes)
	require.Equal(t, second, root.paneFocus())
	requireWorkspaceMembership(t, root)
}

func TestPaneWorkspaceMoveRemoveAndSinglePreserveCompanions(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.handleSwitchTab("third")
	root.splitPane("second", "third", splitBottom)
	require.Equal(t, []string{"third", "second"}, root.panes.Sessions())
	requireWorkspaceMembership(t, root)
	root.handleSwitchTab("profile")
	require.Equal(t, []string{"profile"}, root.panes.Sessions())
	root.handleSwitchTab("second")
	root.removePane("second")
	require.Equal(t, []string{"third"}, root.panes.Sessions())
	root.handleSwitchTab("second")
	require.Equal(t, []string{"second"}, root.panes.Sessions())
	root.splitPane("profile", "second", splitLeft)
	root.splitPane("third", "profile", splitBottom)
	companions, ok := root.panes.Remove("third")
	require.True(t, ok)
	root.singlePane()
	require.Equal(t, []string{"third"}, root.panes.Sessions())
	root.handleSwitchTab("profile")
	require.Equal(t, companions, root.panes)
	require.Equal(t, 3, root.supervisor.Count())
	requireWorkspaceMembership(t, root)
}

func TestPaneWorkspaceCloseHiddenRoutesPrunesSavedLayouts(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.handleSwitchTab("third")
	root.closeTab("profile")
	require.Equal(t, []string{"third"}, root.panes.Sessions())
	require.NotContains(t, root.paneWorkspaces.byRoute, "profile")
	root.handleSwitchTab("second")
	require.Equal(t, []string{"second"}, root.panes.Sessions())
	root.handleSwitchTab("third")
	root.closeTab("second")
	require.Len(t, root.paneWorkspaces.byRoute, 1)
	requireWorkspaceMembership(t, root)
}

func TestPaneWorkspaceRouteReplacementKeepsGeometryAndUniqueMembership(t *testing.T) {
	for _, active := range []string{"profile", "third"} {
		t.Run(active, func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.handleSwitchTab("third")
			root.handleSwitchTab(active)
			root.replacePaneWorkspaceRoute("second", "third")
			root.handleSwitchTab("third")
			require.Equal(t, []string{"profile", "third"}, root.panes.Sessions())
			require.NotContains(t, root.paneWorkspaces.byRoute, "second")
			requireWorkspaceMembership(t, root)
		})
	}
}

func TestPaneWorkspaceRoundTripInvalidatesPendingEdits(t *testing.T) {
	t.Run("divider and picker", func(t *testing.T) {
		root := splitTestRoot(t)
		root.splitPane("second", "profile", splitRight)
		guard := root.capturePaneChoiceGuard()
		before := root.panes
		d := root.paneGeometry.Dividers[0]
		root.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: d.ID})
		root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
		root.handleSwitchTab("third")
		root.handleSwitchTab("second")
		require.Nil(t, root.paneGesture)
		require.False(t, root.validPaneChoiceGuard(guard))
		require.Equal(t, before, root.panes)
		root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		require.Equal(t, before, root.panes)
	})
	t.Run("hydration", func(t *testing.T) {
		root, _ := coldPaneRoot(t)
		root.splitPane("second", "profile", splitRight)
		before := root.panes
		cmd := root.splitPane("cold", "profile", splitBottom)
		require.NotNil(t, root.paneHydration)
		root.handleSwitchTab("third")
		root.handleSwitchTab("second")
		root.finishPaneHydration(cmd().(paneHydratedMsg))
		require.Nil(t, root.paneHydration)
		require.Nil(t, root.chatPages["cold"])
		require.Equal(t, before, root.panes)
	})
	t.Run("source", func(t *testing.T) {
		root, _ := paneSourceFixture(t)
		root.splitPane("second", "profile", splitRight)
		before := root.panes
		prepared := root.beginPaneSource("closed-source", "profile", splitBottom)().(paneSourcePreparedMsg)
		commit := root.finishPaneSourcePrepared(prepared)
		result := commit().(paneSourceCommittedMsg)
		root.handleSwitchTab("third")
		root.handleSwitchTab("second")
		root.finishPaneSourceCommitted(result)
		require.Nil(t, root.paneSource)
		require.Nil(t, root.supervisor.GetRunner("closed-source"))
		require.Equal(t, before, root.panes)
	})
}
