package tui

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRootTabsFollowPerSessionActiveAgentIdentity(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	child := session.New(session.WithID("full-child-session-not-a-node"))
	info := runtime.SubagentAttachInfo{NodeID: subagent.NodeID("full-canonical-child-node-identity"), Agent: "planner", Session: child}
	application := app.New(t.Context(), nil, child, runtime.SessionBinding{AgentName: "planner"}, app.WithRuntimeServices(stubRuntime{}), app.WithSubagentAttach(info))
	_, err := root.supervisor.AddSession(t.Context(), application, child, "", nil)
	require.NoError(t, err)
	page, state, ed, activeApp := root.chatPage, root.sessionState, root.editor, root.application
	root.initSessionComponents(child.ID, application, child)
	root.chatPage, root.sessionState, root.editor, root.application = page, state, ed, activeApp
	tabs, active := root.supervisor.GetTabs()
	_, _ = root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	require.Equal(t, "root", root.tabInfos[0].AgentName)
	require.Equal(t, string(subagent.SessionRootID("profile")), root.tabInfos[0].AgentNodeID)
	require.Equal(t, "planner", root.tabInfos[1].AgentName)
	require.Equal(t, string(info.NodeID), root.tabInfos[1].AgentNodeID)
	_, _ = root.Update(messages.RoutedMsg{SessionID: child.ID, Inner: runtime.AgentInfo("reviewer", "provider/model", "", "")})
	require.Equal(t, "root", root.tabInfos[0].AgentName)
	require.Equal(t, "reviewer", root.tabInfos[1].AgentName)
	require.Equal(t, string(info.NodeID), root.tabInfos[1].AgentNodeID, "agent transfer does not invent a new session node")
	_, _ = root.Update(messages.RoutedMsg{SessionID: child.ID, Inner: &runtime.TokenUsageEvent{SessionID: child.ID, AgentContext: runtime.AgentContext{AgentName: "other"}, Usage: &runtime.Usage{ContextLength: 1}}})
	require.Equal(t, "reviewer", root.tabInfos[1].AgentName, "accounting never switches the active agent")
	view := root.tabBar.View()
	require.Contains(t, ansi.Strip(view), "R", "active agent initial is visible")
	pillFound := false
	for _, cell := range chromeCells(view) {
		if cell.glyph == "R" && cell.bg != nil && color.NRGBAModel.Convert(cell.bg) == color.NRGBAModel.Convert(styles.AgentIdentityStyle("root", false).GetForeground()) {
			pillFound = true
		}
	}
	require.True(t, pillFound, "tab initial uses the same semantic agent color as the sidebar")
	require.NotContains(t, ansi.Strip(root.tabBar.View()), string(info.NodeID), "full identity is metadata, not a tab title")
}

func TestRootThemeGenerationInvalidatesCompleteView(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	root, _, _ := wallClockRoot(t, 120, 40)
	before := root.View()
	generation := root.viewThemeGeneration
	target := "default-light"
	if original.Ref == target {
		target = "default"
	}
	theme, err := styles.LoadTheme(target)
	require.NoError(t, err)
	styles.ApplyTheme(theme)
	require.Greater(t, styles.ThemeGeneration(), generation)
	after := root.View()
	require.NotEqual(t, before.Content, after.Content)
	require.Equal(t, styles.ThemeGeneration(), root.viewThemeGeneration)
	_, _ = root.applyThemeChanged()
	require.False(t, root.viewCacheValid, "explicit theme delivery also invalidates root")
	after = root.View()
	rows := layoutTerminalCells(after.Content)
	x, y := tabFrameOrigin(), root.contentHeight+1
	require.Equal(t, color.NRGBAModel.Convert(styles.EditorBg), color.NRGBAModel.Convert(rows[y][x+1].Style.Bg), "active tab shares the editor surface after a theme change")
	require.Equal(t, after.Content, root.View().Content, "unchanged generation retains the exact complete view")
}

func TestRootResizePointerSameRowKeepsCacheAndCompletionAnchor(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(completion.OpenMsg{Items: []completion.Item{{Label: "completion-anchor", Value: "completion-anchor"}}})
	_, _ = root.Update(tea.MouseClickMsg{X: 40, Y: root.contentHeight, Button: tea.MouseLeft})
	for _, lines := range []int{7, 8, 6, 9} {
		y := root.height - (lines - 1) - 1 - root.tabBar.Height() - root.editor.BannerHeight() - styles.EditorStyle.GetVerticalFrameSize() - 1 - root.messageBarHeight()
		motion := tea.MouseMotionMsg{X: 40, Y: y, Button: tea.MouseLeft}
		_, _ = root.Update(messages.PointerUpdateMsg{X: 40, Y: y, Motion: &motion})
		require.Equal(t, lines-1, root.editorHeight)
		require.False(t, root.editorHeightMotion.Running())
		view := root.View()
		require.Len(t, strings.Split(view.Content, "\n"), root.height)
		layers := root.completions.GetLayers()
		require.Len(t, layers, 1)
		popupTop := layers[0].GetY()
		popupBottom := popupTop + layers[0].Height()
		geometry := root.composerLayout()
		require.Equal(t, geometry.bannerTop-1, popupBottom, "completion stays above composer chrome for editor allocation %d", lines)
		require.GreaterOrEqual(t, popupTop, 0)
		require.Equal(t, regionContent, root.hitTestRegion(popupBottom-1), "last popup row belongs to the transcript")
		rows := strings.Split(view.Content, "\n")
		require.Contains(t, ansi.Strip(strings.Join(rows[popupTop:popupBottom], "\n")), "completion-anchor", "actual composed label is within the popup layer")
		require.Equal(t, ansi.Strip(root.renderResizeHandle(root.width)), ansi.Strip(rows[geometry.separatorTop]), "completion does not overwrite the real resize separator")
		require.NotContains(t, ansi.Strip(rows[geometry.tabsTop]), "completion-anchor", "completion does not overwrite the tab row")
		_, _ = root.Update(messages.PointerUpdateMsg{X: 40, Y: y, Motion: &motion})
		require.True(t, root.viewCacheValid, "same accepted pointer row must not invalidate root")
		require.Equal(t, view.Content, root.View().Content)
	}
	_, _ = root.Update(tea.MouseReleaseMsg{X: 40, Y: -1, Button: tea.MouseLeft})
	require.False(t, root.isDragging)
	require.False(t, root.editorHeightMotion.Running())
}

func prepareRootImmediateChrome(root *appModel) {
	root.ar.Stop()
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	root.dialogMgr = dialog.New(root.ar)
	root.tabBar = tabbar.New(root.ar, 0)
	root.tabBar.SetWidth(tabFrameWidth(root.width))
	root.tabBar.SetTabs(root.tabInfos, 0)
	root.viewCacheValid = false
}

func settleRootChrome(t *testing.T, root *appModel, cmd tea.Cmd) {
	t.Helper()
	pending := collectMsgs(cmd)
	for step := 0; step < 40 && root.ar.ActiveCount() > 0; step++ {
		var next []tea.Msg
		for _, msg := range pending {
			if _, ok := msg.(animation.TickMsg); ok {
				_, cmd := root.Update(msg)
				next = append(next, collectMsgs(cmd)...)
			}
		}
		pending = next
	}
	require.Zero(t, root.ar.ActiveCount(), "original chrome command chain must settle")
}

func TestRootSingleTabPlusHoverLeaveAndOutsideRelease(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	prepareRootImmediateChrome(root)
	plain := ansi.Strip(root.tabBar.View())
	index := strings.LastIndex(plain, "+")
	require.GreaterOrEqual(t, index, 0)
	x, y := ansi.StringWidth(plain[:index])+tabFrameOrigin(), root.contentHeight+1
	idle := root.View().Content
	_, hoverCmd := root.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, idle, root.View().Content, "hover starts at currentcolor")
	settleRootChrome(t, root, hoverCmd)
	hovered := root.View().Content
	require.NotEqual(t, idle, hovered, "root forwards plus hover in tab-local cells")
	_, leaveCmd := root.Update(tea.MouseMotionMsg{X: x, Y: y - 2})
	settleRootChrome(t, root, leaveCmd)
	require.Equal(t, idle, root.View().Content, "leave clears plus hover")
	_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Contains(t, collectMsgs(cmd), tea.Msg(messages.SpawnSessionMsg{}))
	require.True(t, root.tabBar.HasPointerCapture())
	_, cmd = root.Update(tea.MouseReleaseMsg{X: x, Y: y - 2, Button: tea.MouseLeft})
	require.NotContains(t, collectMsgs(cmd), tea.Msg(messages.SpawnSessionMsg{}), "release never duplicates the accepted spawn")
	require.False(t, root.tabBar.HasPointerCapture(), "outside release reaches the pressed plus")
	_, _ = root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, _ = root.Update(tea.BlurMsg{})
	require.False(t, root.tabBar.HasPointerCapture(), "blur clears capture")
}

func TestRootBackgroundDialogTabPlusHoverLeavesAfterRelease(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	prepareRootImmediateChrome(root)
	_, openCmd := root.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(help.Document{}), OriginatingEvent: "background-tab-hover"})
	settleRootChrome(t, root, openCmd)
	require.True(t, root.dialogMgr.TopIsBackground())
	idle := root.tabBar.View()
	plain := ansi.Strip(idle)
	index := strings.LastIndex(plain, "+")
	require.GreaterOrEqual(t, index, 0)
	x, y := ansi.StringWidth(plain[:index])+tabFrameOrigin(), root.contentHeight+1
	_, hoverCmd := root.Update(tea.MouseMotionMsg{X: x, Y: y})
	settleRootChrome(t, root, hoverCmd)
	require.NotEqual(t, idle, root.tabBar.View(), "background dialog permits tab hover")
	_, clickCmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, _ = root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.False(t, root.tabBar.HasPointerCapture())
	_, _ = root.Update(tea.MouseMotionMsg{X: x, Y: y - 2})
	require.NotEqual(t, idle, root.tabBar.View(), "release and leave do not snap the accepted pulse")
	pending := collectMsgs(clickCmd)
	for step := 0; step < 30 && root.tabBar.IsAnimating(); step++ {
		var next []tea.Msg
		for _, msg := range pending {
			if _, ok := msg.(animation.TickMsg); ok {
				_, cmd := root.Update(msg)
				next = append(next, collectMsgs(cmd)...)
			}
		}
		pending = next
	}
	require.False(t, root.tabBar.IsAnimating())
	require.Equal(t, idle, root.tabBar.View(), "finite feedback settles outside behind a background dialog")
}

func TestRootTinyTabAllocationDoesNotUseFallbackWidth(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	for _, width := range []int{0, 1, 2, 3, 4, 8} {
		_, _ = root.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		require.LessOrEqual(t, ansi.StringWidth(root.tabBar.View()), tabFrameWidth(width), "allocated tab width %d", width)
		if tabFrameWidth(width) == 0 {
			_, cmd := root.Update(tea.MouseClickMsg{X: tabFrameOrigin(), Y: root.contentHeight + 1, Button: tea.MouseLeft})
			require.Nil(t, cmd, "zero-width tab strip has no pointer targets")
		}
	}
}
