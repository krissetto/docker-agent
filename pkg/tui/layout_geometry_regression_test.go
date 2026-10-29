package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func useVisibleGeometryChatPage(t *testing.T, root *appModel, settings messages.LayoutSettings) {
	t.Helper()
	page := chat.New(root.ar, t.Context(), root.application, root.sessionState, chat.WithLayoutSettings(settings))
	page.SetRoutingID(root.paneFocus())
	root.chatPage = page
	root.chatPages[root.paneFocus()] = page
	root.layoutSettings = settings
	root.hideSidebar = false
	root.viewCacheValid = false
}

func TestFullTUITopInsetAndAlignedComposerGeometry(t *testing.T) {
	for _, position := range []messages.SidebarPosition{messages.SidebarLeft, messages.SidebarRight} {
		t.Run(string(position), func(t *testing.T) {
			root := splitTestRoot(t)
			settings := messages.LayoutSettings{SidebarPosition: position, AlignInputWithMessages: true}
			useVisibleGeometryChatPage(t, root, settings)
			root.applyLayoutSettings(settings)
			root.editor.SetValue("aligned draft")
			root.resizeAll()

			column := root.composerColumn()
			shell := root.chatPage.(chat.SplitPresentation).MeasureSplitShell(root.width, root.mainContentHeight())
			require.Greaterf(t, shell.SidebarHandle.Height, 1, "fixture has a vertical sidebar: width=%d content=%d shell=%+v sidebar=%+v view=%q", root.width, root.contentHeight, shell, root.chatPage.GetSidebarSettings(), root.chatPage.(chat.SplitPresentation).SidebarView())
			require.Equal(t, shell.TranscriptArea.X, column.X)
			require.Equal(t, shell.TranscriptArea.Width, column.W)
			require.Equal(t, 1, root.mainContentInset(), "full-TUI transcript reserves one app-owned row")
			require.Equal(t, root.contentHeight, root.composerLayout().bannerTop, "the inset does not move composer/footer coordinates")
			require.False(t, root.chatPage.(interface{ PointerTargetsMessages(x, y int) bool }).PointerTargetsMessages(column.X, 0), "the blank top row has no transcript hit target")
			require.True(t, root.chatPage.(interface{ PointerTargetsMessages(x, y int) bool }).PointerTargetsMessages(column.X, 1))

			g := root.composerLayout()
			require.True(t, root.composerResizeHit(column.X, g.separatorTop))
			require.False(t, root.composerResizeHit(max(0, column.X-1), g.separatorTop), "resize hit matches transcript/composer column")
			root.Update(tea.MouseClickMsg{X: max(0, column.X-1), Y: g.separatorTop, Button: tea.MouseLeft})
			require.False(t, root.isDragging, "outside-column separator clicks do not resize the composer")

			frame := strings.Split(ansi.Strip(root.View().Content), "\n")
			require.Len(t, frame, root.height)
			require.Equal(t, strings.Repeat(" ", root.width), frame[0], "the entire main surface starts with a blank row")
			inputRow := -1
			for y, line := range frame {
				if strings.Contains(line, "aligned draft") {
					inputRow = y
					break
				}
			}
			require.GreaterOrEqual(t, inputRow, 0, "draft is rendered")
			require.Equal(t, strings.Repeat(" ", column.X), ansi.Cut(frame[inputRow], 0, column.X), "editor text starts in the transcript column")
			require.Equal(t, strings.Repeat(" ", root.width-column.X-column.W), ansi.Cut(frame[inputRow], column.X+column.W, root.width), "the composer does not bleed into the sidebar")

			sidebar, handle, ok := root.sidebarContinuationGeometry()
			require.True(t, ok)
			require.GreaterOrEqual(t, sidebar.Y+sidebar.Height, root.composerLayout().editorBottom, "vertical sidebar extends beside the composer")
			require.True(t, root.sidebarContinuationAt(handle.X, root.composerLayout().editorTop), "composer-row sidebar handle routes to the page")
			require.Equal(t, ansi.Strip(root.renderMessageBar()), frame[root.height-root.messageBarHeight()], "bottom status/footer remains full-width and unchanged")
		})
	}
}

func TestComposerAlignmentFallsBackForNonVerticalAndLeanLayouts(t *testing.T) {
	for _, test := range []struct {
		name     string
		position messages.SidebarPosition
		width    int
		lean     bool
		collapse bool
	}{
		{name: "top dock", position: messages.SidebarTop, width: 120},
		{name: "bottom dock", position: messages.SidebarBottom, width: 120},
		{name: "narrow", position: messages.SidebarRight, width: 80},
		{name: "collapsed", position: messages.SidebarRight, width: 120, collapse: true},
		{name: "lean", position: messages.SidebarRight, width: 120, lean: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := splitTestRoot(t)
			root.handleWindowResize(test.width, 30)
			root.layoutSettings = messages.LayoutSettings{SidebarPosition: test.position, AlignInputWithMessages: true}
			root.applyLayoutSettings(root.layoutSettings)
			root.leanMode = test.lean
			if test.collapse {
				root.chatPage.SetSidebarSettings(chat.SidebarSettings{Collapsed: true, PreferredWidth: 40})
			}
			root.resizeAll()
			require.Equal(t, splitRect{W: root.width}, root.composerColumn(), "layouts without a vertical message column keep the default composer")
			require.Equal(t, tabFrameOrigin(), root.tabOrigin(), "the tab strip keeps the full-width frame")
			require.Equal(t, tabFrameWidth(root.width), root.tabWidth())
			require.Equal(t, test.lean, root.mainContentInset() == 0, "lean mode is the only full-layout exception to the top inset")
		})
	}
}

// Real sidebar rows distinguish continuation from blank padding.
func alignedDockFixture(t *testing.T, position messages.SidebarPosition, draft string) *appModel {
	t.Helper()
	root := splitTestRoot(t)
	settings := messages.LayoutSettings{SidebarPosition: position, AlignInputWithMessages: true}
	useVisibleGeometryChatPage(t, root, settings)
	root.applyLayoutSettings(settings)
	todos := make([]session.Todo, 0, 40)
	for i := range 40 {
		todos = append(todos, session.Todo{ID: fmt.Sprintf("row-%02d", i), Description: fmt.Sprintf("sidebar row %02d", i), Status: "pending"})
	}
	root.chatPage.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{Owner: root.paneFocus(), SessionID: root.application.Session().ID}, Todos: todos})
	// The frozen test clock never finishes the sidebar's row placement; jump it to rest.
	chat.CancelSidebarPresentation(root.chatPage)
	root.editor.SetValue(draft)
	root.resizeAll()
	root.View()
	expandSidebarTodos(t, root)
	return root
}

func expandSidebarTodos(t *testing.T, root *appModel) {
	t.Helper()
	column, g := root.composerColumn(), root.composerLayout()
	base, width := 0, column.X
	if root.layoutSettings.SidebarPosition != messages.SidebarLeft {
		base, width = column.X+column.W, root.width-column.X-column.W
	}
	for y, line := range strings.Split(ansi.Strip(root.View().Content), "\n")[:g.tabsTop] {
		side := ansi.Cut(line, base, base+width)
		if idx := strings.Index(side, "todos"); idx >= 0 {
			root.Update(tea.MouseClickMsg{X: base + ansi.StringWidth(side[:idx]), Y: y, Button: tea.MouseLeft})
			chat.CancelSidebarPresentation(root.chatPage)
			root.View()
			return
		}
	}
	t.Fatal("sidebar todo summary is rendered")
}

func assertAlignedDock(t *testing.T, root *appModel, visible string) {
	t.Helper()
	column, g := root.composerColumn(), root.composerLayout()
	position := root.layoutSettings.SidebarPosition
	require.Less(t, column.W, root.width, "aligned layout narrows the dock")
	require.Positive(t, root.contextHeight)
	require.Equal(t, column.X+tabFrameOrigin(), root.tabOrigin(), "tabs start at the dock column")
	require.Equal(t, tabFrameWidth(column.W), root.tabWidth(), "tabs are as wide as the dock column")

	frame := strings.Split(ansi.Strip(root.View().Content), "\n")
	require.Len(t, frame, root.height)
	footer := root.height - root.messageBarHeight()
	require.Equal(t, ansi.Strip(root.renderMessageBar()), frame[footer], "the global footer stays full width")

	sidebarSide := func(row string) string {
		if position == messages.SidebarLeft {
			return ansi.Cut(row, 0, column.X)
		}
		return ansi.Cut(row, column.X+column.W, root.width)
	}
	dockSide := func(row string) string { return ansi.Cut(row, column.X, column.X+column.W) }
	for y := g.tabsTop; y < footer; y++ {
		// The sidebar keeps its bottom badge row and one spacer above it.
		if y < footer-2 {
			require.NotEmptyf(t, strings.TrimSpace(sidebarSide(frame[y])), "sidebar has real content beside row %d", y)
		}
		require.True(t, root.sidebarContinuationAt(sidebarContinuationX(t, root), y), "sidebar pointer routing covers row %d", y)
	}

	tabsDock := dockSide(frame[g.tabsTop])
	require.Contains(t, tabsDock, "profile", "tab labels render inside the dock")
	require.NotContains(t, sidebarSide(frame[g.tabsTop]), "profile", "tab labels do not leak beside the sidebar")
	idx := strings.Index(tabsDock, "profile")
	_, hit := root.tabBar.TabBodyAt(column.X+ansi.StringWidth(tabsDock[:idx])-root.tabOrigin(), 0)
	require.True(t, hit, "the rendered tab label is a tab-bar pointer target in the dock column")

	if visible != "" {
		draftRow := -1
		for y := g.editorTop; y < g.editorBottom && draftRow < 0; y++ {
			if strings.Contains(dockSide(frame[y]), visible) {
				draftRow = y
			}
		}
		require.GreaterOrEqual(t, draftRow, 0, "composer text renders inside the dock")
		require.NotContains(t, sidebarSide(frame[draftRow]), visible)
	}
	contextRow := g.contextBottom - 1
	require.Contains(t, dockSide(frame[contextRow]), "Context", "context usage label renders inside the dock")
	require.NotContains(t, sidebarSide(frame[contextRow]), "Context", "context usage label does not paint beside the sidebar")
	require.NotContains(t, sidebarSide(frame[contextRow]), "▄", "context track does not paint beside the sidebar")
	require.False(t, root.sidebarContinuationAt(column.X+1, g.tabsTop), "dock cells are not sidebar pointer targets")
}

func sidebarContinuationX(t *testing.T, root *appModel) int {
	t.Helper()
	sidebar, _, ok := root.sidebarContinuationGeometry()
	require.True(t, ok)
	return sidebar.X
}

func TestAlignedComposerDockSharesOneColumnBesideSidebar(t *testing.T) {
	for _, position := range []messages.SidebarPosition{messages.SidebarLeft, messages.SidebarRight} {
		t.Run(string(position), func(t *testing.T) {
			assertAlignedDock(t, alignedDockFixture(t, position, "aligned draft"), "aligned draft")
		})
	}
}

func TestAlignedComposerDockGrowsShrinksAndTogglesWithoutLeakingIntoSidebar(t *testing.T) {
	root := alignedDockFixture(t, messages.SidebarLeft, "one")
	root.ar.Stop()
	scheduler := &composerShrinkScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root.ar = animation.NewRuntimeWithScheduler(scheduler)
	assertAlignedDock(t, root, "one")

	root.editor.SetValue("aligned draft\ntwo\nthree\nfour\nfive")
	root.resizeAll()
	root.View()
	assertAlignedDock(t, root, "five")

	from := root.editorHeight
	root.Update(messages.RestorePendingMessagesMsg{Content: "one"})
	assertComposerDelayedShrink(t, root, scheduler, from, 1)
	assertAlignedDock(t, root, "one")

	root.applyLayoutSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarLeft})
	root.View()
	require.Equal(t, splitRect{W: root.width}, root.composerColumn(), "turning alignment off restores the full-width dock")
	require.Equal(t, tabFrameOrigin(), root.tabOrigin())
	require.Equal(t, tabFrameWidth(root.width), root.tabWidth())
	g := root.composerLayout()
	require.Contains(t, ansi.Strip(root.View().Content), "Context", "context usage still renders full width")
	require.Equal(t, root.width, lipgloss.Width(strings.Split(root.View().Content, "\n")[g.editorBottom]), "context row is full width when off")

	root.applyLayoutSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarLeft, AlignInputWithMessages: true})
	root.resizeAll()
	root.View()
	assertAlignedDock(t, root, "one")
}

func TestAlignInputRenderedDockFixtures(t *testing.T) {
	fixtureDir := os.Getenv("DOCKER_AGENT_LAYOUT_FIXTURE_DIR")
	if fixtureDir == "" {
		t.Skip("set DOCKER_AGENT_LAYOUT_FIXTURE_DIR to render left/right appearance fixtures")
	}
	require.NoError(t, os.MkdirAll(fixtureDir, 0o700))
	for _, position := range []messages.SidebarPosition{messages.SidebarLeft, messages.SidebarRight} {
		root := alignedDockFixture(t, position, "aligned input column")
		root.chatPage.Update(runtime.UserMessage("message body follows a blank top row", root.application.Session().ID, nil))
		root.sessionState.SetYoloMode(true)
		root.resizeAll()
		frame := ansi.Strip(root.View().Content)
		path := filepath.Join(fixtureDir, fmt.Sprintf("%s.txt", position))
		require.NoError(t, os.WriteFile(path, []byte(frame+"\n"), 0o600))
		ansiPath := filepath.Join(fixtureDir, fmt.Sprintf("%s.ansi.txt", position))
		require.NoError(t, os.WriteFile(ansiPath, []byte(root.View().Content+"\n"), 0o600))
		require.Contains(t, frame, "aligned input column")
		require.Equal(t, strings.Repeat(" ", root.width), strings.Split(frame, "\n")[0])
	}
}

func TestAlignedSplitAndReviewSurfacesKeepSingleTopInset(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		root := splitTestRoot(t)
		settings := messages.LayoutSettings{SidebarPosition: messages.SidebarRight, AlignInputWithMessages: true}
		useVisibleGeometryChatPage(t, root, settings)
		root.applyLayoutSettings(settings)
		root.splitPane("profile", "second", splitRight)
		require.GreaterOrEqual(t, root.paneShell.TranscriptArea.Y, 1, "the app-level inset is outside each pane")
		for _, rect := range root.paneGeometry.Panes {
			require.GreaterOrEqual(t, rect.Y, 1)
		}
		require.True(t, root.sidebarContinuationAt(root.paneShell.SidebarHandle.X, root.composerLayout().editorTop), "split sidebar handle routes through the composer continuation")
		frame := strings.Split(ansi.Strip(root.View().Content), "\n")
		require.Equal(t, strings.Repeat(" ", root.width), frame[0])
	})
	t.Run("review", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		h.open(true)
		h.m.layoutSettings = messages.LayoutSettings{AlignInputWithMessages: true}
		h.m.resizeAll()
		frame := strings.Split(ansi.Strip(h.m.composeView().Content), "\n")
		require.Equal(t, strings.Repeat(" ", h.m.width), frame[0], "review surface gets the same one-row app inset")
		require.Contains(t, strings.Join(frame[1:], "\n"), "README.md")
	})
}

func TestAlignInputComposerAndAttachmentInteractionsStayColumnLocal(t *testing.T) {
	root := splitTestRoot(t)
	settings := messages.LayoutSettings{SidebarPosition: messages.SidebarLeft, AlignInputWithMessages: true}
	useVisibleGeometryChatPage(t, root, settings)
	root.applyLayoutSettings(settings)
	path := filepath.Join(t.TempDir(), "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("attachment body"), 0o600))
	require.NoError(t, root.editor.AttachFile(path))
	root.editor.SetValue("draft")
	root.resizeAll()
	column, geometry := root.composerColumn(), root.composerLayout()
	require.Positive(t, root.editor.BannerHeight())
	require.Less(t, root.editor.BannerHeight(), root.height)
	require.LessOrEqual(t, lipgloss.Width(root.editor.BannerView(column.W)), column.W)
	previewX, previewY := -1, -1
	for y := range root.editor.BannerHeight() {
		for x := range column.W {
			if _, found := root.editor.AttachmentAtPosition(x, y); found {
				previewX, previewY = x, y
				break
			}
		}
		if previewX >= 0 {
			break
		}
	}
	require.GreaterOrEqual(t, previewX, 0, "expanded banner has an attachment hit target")
	root.handleMouseClick(tea.MouseClickMsg{X: column.X + previewX, Y: geometry.bannerTop + previewY, Button: tea.MouseLeft})
	require.True(t, root.dialogMgr.Open(), "banner previews retain local X coordinates after composer translation")
}

func frameLines(root *appModel) []string {
	return strings.Split(ansi.Strip(root.View().Content), "\n")
}

func dockTabRow(root *appModel) string {
	column, g := root.composerColumn(), root.composerLayout()
	return ansi.Cut(frameLines(root)[g.tabsTop], column.X, column.X+column.W)
}

func dockTabX(t *testing.T, root *appModel, text string) int {
	t.Helper()
	column := root.composerColumn()
	dock := dockTabRow(root)
	idx := strings.Index(dock, text)
	require.GreaterOrEqual(t, idx, 0, "%q is rendered in the dock tab row", text)
	return column.X + ansi.StringWidth(dock[:idx])
}

func TestAlignedComposerDockTabPointerRoutesUseDockColumn(t *testing.T) {
	root := alignedDockFixture(t, messages.SidebarLeft, "one")
	g := root.composerLayout()

	// A press on a registered session's tab starts a pane gesture at the dock's tab row.
	x := dockTabX(t, root, "▎ 会話 λ second") // the accent column is inside the tab
	_, _ = root.Update(tea.MouseClickMsg{X: x, Y: g.tabsTop, Button: tea.MouseLeft})
	require.NotNil(t, root.paneGesture, "a press on a dock tab label starts a tab gesture")
	require.Equal(t, "second", root.paneGesture.source)
	_, cmd := root.Update(tea.MouseReleaseMsg{X: x, Y: g.tabsTop, Button: tea.MouseLeft})
	_, switched := firstOfType[messages.SwitchTabMsg](collectMsgs(cmd))
	require.True(t, switched || root.paneFocus() == "second", "releasing on the dock tab label selects that tab")
	require.Nil(t, root.paneGesture)

	closeX := dockTabX(t, root, "×")
	_, cmd = root.Update(tea.MouseClickMsg{X: closeX, Y: g.tabsTop, Button: tea.MouseLeft})
	closeMsg, ok := firstOfType[messages.CloseTabMsg](collectMsgs(cmd))
	require.True(t, ok, "the close button in the dock column reaches the tab bar")
	require.Equal(t, root.tabInfos[0].SessionID, closeMsg.SessionID)
}

func TestAlignedComposerDockTabWheelScrollsOnlyInsideDockColumn(t *testing.T) {
	root := alignedDockFixture(t, messages.SidebarLeft, "one")
	g, column := root.composerLayout(), root.composerColumn()
	tabs := append([]messages.TabInfo(nil), root.tabInfos...)
	for i := range 12 {
		tabs = append(tabs, messages.TabInfo{SessionID: fmt.Sprintf("overflow-%02d", i), Title: fmt.Sprintf("Overflow %02d", i)})
	}
	root.tabBar.SetTabs(tabs, 0)
	root.viewCacheValid = false
	root.resizeAll()
	root.View()

	before := dockTabRow(root)
	wheel := func(x int) {
		for range 3 {
			root.Update(messages.PointerUpdateMsg{X: x, Y: g.tabsTop, HasWheel: true, WheelHorizontal: true, WheelDelta: 1})
		}
		root.Update(tea.MouseReleaseMsg{X: x, Y: g.tabsTop, Button: tea.MouseLeft})
		root.View()
	}
	wheel(column.X + 4)
	scrolled := dockTabRow(root)
	require.NotEqual(t, before, scrolled, "horizontal wheel over the dock tab strip scrolls it")

	sidebar, _, ok := root.sidebarContinuationGeometry()
	require.True(t, ok)
	wheel(sidebar.X + 1)
	require.Equal(t, scrolled, dockTabRow(root), "wheel beside the dock never scrolls its tab strip")
}

func TestAlignedComposerDockTabGestureUsesBannerRow(t *testing.T) {
	root := alignedDockFixture(t, messages.SidebarLeft, "draft")
	path := filepath.Join(t.TempDir(), "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("attachment body"), 0o600))
	require.NoError(t, root.editor.AttachFile(path))
	root.resizeAll()
	root.View()
	g := root.composerLayout()
	require.Positive(t, root.editor.BannerHeight(), "the attachment banner sits above the tabs")

	x := dockTabX(t, root, "second")
	require.True(t, root.beginPaneGesture(tea.MouseClickMsg{X: x, Y: g.tabsTop, Button: tea.MouseLeft}),
		"a press on the tab row starts a tab gesture below the attachment banner")
	root.cancelPaneGesture()
}

func TestAlignedComposerDockSplitSharesTranscriptColumn(t *testing.T) {
	root := alignedDockFixture(t, messages.SidebarRight, "split draft")
	root.splitPane("profile", "second", splitRight)
	root.editor.SetValue("split draft")
	root.resizeAll()
	root.View()

	assertAlignedDock(t, root, "split draft")
	column, g := root.composerColumn(), root.composerLayout()
	require.Equal(t, root.paneShell.TranscriptArea.X, column.X, "the split dock starts at the shared transcript column")
	require.Equal(t, root.paneShell.TranscriptArea.Width, column.W, "the split dock is as wide as the shared transcript column")
	frame := frameLines(root)
	require.Equal(t, ansi.Strip(root.renderMessageBar()), frame[root.height-root.messageBarHeight()], "the footer stays full width")
	require.Contains(t, dockTabRow(root), "profile", "split tabs render inside the dock column")
	require.Contains(t, ansi.Cut(frame[g.contextBottom-1], column.X, column.X+column.W), "Context")
	require.NotContains(t, ansi.Cut(frame[g.contextBottom-1], column.X+column.W, root.width), "Context", "context usage stays out of the sidebar side")
	for y := g.tabsTop; y < root.height-root.messageBarHeight(); y++ {
		require.True(t, root.sidebarContinuationAt(root.paneShell.SidebarHandle.X, y), "split sidebar routing covers dock row %d", y)
	}
}

// Real chat pages support pane presentation, so reviewCanDock excludes them.
func TestAlignedComposerDockUndockedReviewKeepsDockColumn(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(true)
	settings := messages.LayoutSettings{SidebarPosition: messages.SidebarRight, AlignInputWithMessages: true}
	useVisibleGeometryChatPage(t, h.m, settings)
	h.m.applyLayoutSettings(settings)
	require.NotNil(t, h.m.reviewSurface(), "the review surface stays open")
	require.False(t, h.m.reviewDocked())
	h.m.editor.SetValue("review draft")
	h.m.resizeAll()
	h.m.View()

	column, g := h.m.composerColumn(), h.m.composerLayout()
	require.Less(t, column.W, h.m.width, "the composer is aligned beneath the review surface")
	frame := strings.Split(ansi.Strip(h.m.composeView().Content), "\n")
	require.Contains(t, ansi.Cut(frame[g.contextBottom-1], column.X, column.X+column.W), "Context", "context usage renders in the chat column")
	require.Equal(t, ansi.Strip(h.m.renderMessageBar()), frame[h.m.height-h.m.messageBarHeight()], "the footer stays full width beneath a review")
	for y := g.tabsTop; y < h.m.height-h.m.messageBarHeight(); y++ {
		require.True(t, h.m.sidebarContinuationAt(h.m.paneShell.SidebarHandle.X, y), "sidebar routing covers dock row %d", y)
	}
}

func TestAlignedComposerColumnMeasuresOnFirstResize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sess := session.New(session.WithID("profile"))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	settings := messages.LayoutSettings{SidebarPosition: messages.SidebarLeft, AlignInputWithMessages: true}
	root := New(t.Context(), nil, application, home, nil, func(m *appModel) {
		m.layoutSettings = settings
	}).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	require.Zero(t, root.contentHeight)
	require.Zero(t, root.editorHeight)
	root.Init()
	root.handleWindowResize(120, 40)
	first := root.composerColumn()
	require.Less(t, first.W, root.width, "the initial resize aligns the dock without a prior transcript measurement")
	require.Equal(t, root.paneShell.TranscriptArea.X, first.X)
	require.Equal(t, root.paneShell.TranscriptArea.Width, first.W)
	require.Equal(t, first.W, lipgloss.Width(root.composerView()), "the first editor render uses the measured dock width")
	require.Equal(t, first.W, lipgloss.Width(root.composerDockView(root.tabBar.View(), root.composerView())))
	root.resizeAll()
	require.Equal(t, first, root.composerColumn(), "the first resize and later resizes measure the same column")
	root.handleWindowResize(80, 30)
	require.Equal(t, splitRect{W: 80}, root.composerColumn(), "narrow layouts fall back to a full-width dock")
	root.handleWindowResize(120, 40)
	require.Equal(t, first, root.composerColumn(), "widening restores alignment")
}
