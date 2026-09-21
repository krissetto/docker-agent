package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// Preserve the real page's optional geometry/capture capabilities while
// counting the releases that reach precisely this canonical page.
type scrollbarCapturePage struct {
	chat.Page
	chat.SplitPresentation
	chat.SplitSidebarPresentation
	chat.PresentationVisibility
	chat.PresentationSelection
	messagesScrollbarOwner

	presses, releases int
	pressedDragging   bool
	pressedAt         tea.MouseClickMsg
}

func (p *scrollbarCapturePage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if _, release := msg.(tea.MouseReleaseMsg); release {
		p.releases++
	}
	updated, cmd := p.Page.Update(msg)
	p.Page = updated.(chat.Page)
	if press, ok := msg.(tea.MouseClickMsg); ok {
		p.presses++
		p.pressedAt = press
		p.pressedDragging = p.IsMessagesScrollbarDragging()
	}
	return p, cmd
}

func scrollbarCaptureRoot(t *testing.T, lean, split bool) (*appModel, *scrollbarCapturePage) {
	t.Helper()
	root := splitTestRoot(t)
	root.leanMode = lean
	root.hideSidebar = false
	for _, id := range []string{"profile", "second"} {
		application := root.supervisor.GetRunner(id).App
		history, _, _ := mixedHistorySession(40)
		application.Session().Messages = history.Messages
		root.createSessionComponents(id, application, application.Session())
		root.chatPages[id].Init()
	}
	root.chatPage, root.editor, root.sessionState = root.chatPages["profile"], root.editors["profile"], root.sessionStates["profile"]
	if split {
		root.splitPane("second", "profile", splitRight)
		root.handleSwitchTab("profile")
	}
	page := root.chatPage
	owner, ok := page.(messagesScrollbarOwner)
	require.True(t, ok)
	spy := &scrollbarCapturePage{
		Page: page, SplitPresentation: page.(chat.SplitPresentation), messagesScrollbarOwner: owner,
		SplitSidebarPresentation: page.(chat.SplitSidebarPresentation), PresentationVisibility: page.(chat.PresentationVisibility),
		PresentationSelection: page.(chat.PresentationSelection),
	}
	root.chatPage, root.chatPages[root.paneFocus()] = spy, spy
	root.editor.SetValue("UNCHANGED DRAFT")
	installComposerInlineBanner(root)
	root.resizeAll()
	root.chatPage.ScrollToBottom()
	root.View()
	return root, spy
}

func startRootScrollbarCapture(t *testing.T, root *appModel, page *scrollbarCapturePage) (int, int) {
	t.Helper()
	geometry := page.MeasureSplitShell(root.width, root.contentHeight).TranscriptArea
	if root.panePresentationEnabled() {
		r := root.paneGeometry.Panes[root.paneFocus()]
		geometry = chat.PresentationRect{X: r.X, Y: r.Y, Width: r.W, Height: r.H - root.paneHeaderHeight()}
	}
	x, y := geometry.X+geometry.Width-1, geometry.Y+geometry.Height-1
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, page.IsMessagesScrollbarDragging(), "real bottom thumb must accept the root click at %d,%d", x, y)
	require.NotNil(t, root.messagesScrollbar)
	require.Same(t, page, root.messagesScrollbar.page)
	return x, y
}

func TestRootMessagesScrollbarReleaseAcrossRegions(t *testing.T) {
	for _, mode := range []struct {
		name        string
		lean, split bool
	}{{name: "full"}, {name: "lean", lean: true}, {name: "split", split: true}} {
		t.Run(mode.name, func(t *testing.T) {
			for _, region := range []string{"same-offset", "input", "banner", "separator", "tabs", "sidebar", "messagebar", "other-pane", "outside"} {
				if mode.lean && (region == "separator" || region == "tabs" || region == "sidebar") || !mode.split && region == "other-pane" {
					continue
				}
				t.Run(region, func(t *testing.T) {
					root, page := scrollbarCaptureRoot(t, mode.lean, mode.split)
					x, y := startRootScrollbarCapture(t, root, page)
					sessionID, panel, height := root.paneFocus(), root.focusedPanel, root.editorHeight
					switch region {
					case "input":
						x, y = 4, root.editorTop()+root.editorFrame().GetPaddingTop()
					case "banner":
						x, y = 4, root.composerLayout().bannerTop+2
					case "separator":
						x, y = 4, root.composerLayout().separatorTop
					case "tabs":
						x, y = tabFrameOrigin()+4, root.composerLayout().tabsTop
					case "sidebar":
						require.Positive(t, root.paneShell.Sidebar.Width)
						require.Positive(t, root.paneShell.Sidebar.Height)
						x, y = root.paneShell.Sidebar.X, root.paneShell.Sidebar.Y
					case "messagebar":
						x, y = messageBarOrigin(root.width)+4, root.height-1
					case "other-pane":
						r := root.paneGeometry.Panes["second"]
						x, y = r.X+r.W/2, r.Y+r.H/2
					case "outside":
						x, y = -20, root.height+20
					}
					root.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
					require.True(t, page.IsMessagesScrollbarDragging(), "motion across shell boundaries retains the original owner")
					root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
					require.False(t, page.IsMessagesScrollbarDragging())
					require.Nil(t, root.messagesScrollbar)
					require.Equal(t, 1, page.releases)
					require.Equal(t, sessionID, root.paneFocus())
					require.Equal(t, panel, root.focusedPanel)
					require.Equal(t, height, root.editorHeight)
					require.Equal(t, "UNCHANGED DRAFT", root.editor.Value())
					require.False(t, root.isDragging)
					require.False(t, root.tabBar.HasPointerCapture())
					require.False(t, root.dialogMgr.Open())
					require.False(t, root.editor.IsContextBarFocused())
					// After release, a normal editor click must not be swallowed
					// by a stale transcript capture.
					root.focusedPanel = PanelContent
					root.Update(tea.MouseClickMsg{X: 4, Y: root.editorTop() + root.editorFrame().GetPaddingTop(), Button: tea.MouseLeft})
					require.Equal(t, PanelEditor, root.focusedPanel)
					require.Nil(t, root.messagesScrollbar)
					require.Equal(t, 1, page.releases)
				})
			}
		})
	}
}

func TestRootMessagesScrollbarCaptureCancellation(t *testing.T) {
	for _, reason := range []string{"blur", "resize", "modal", "hidden", "replaced", "removed"} {
		t.Run(reason, func(t *testing.T) {
			root, page := scrollbarCaptureRoot(t, false, false)
			var scheduler *composerShrinkScheduler
			if reason == "modal" {
				root.ar.Stop()
				scheduler = &composerShrinkScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
				root.ar = animation.NewRuntimeWithScheduler(scheduler)
				root.dialogMgr.Cleanup()
				root.dialogMgr = dialog.New(root.ar)
			}
			startRootScrollbarCapture(t, root, page)
			switch reason {
			case "blur":
				root.Update(tea.BlurMsg{})
			case "resize":
				root.Update(tea.WindowSizeMsg{Width: 100, Height: 35})
			case "modal":
				root.Update(dialog.OpenDialogMsg{Model: &stubDialog{id: "scrollbar-occlusion"}})
			case "hidden":
				root.Update(messages.SwitchTabMsg{SessionID: "second"})
			case "replaced":
				root.chatPages["profile"], root.chatPage = page.Page, page.Page
				root.Update(tea.MouseMotionMsg{X: -1, Y: -1, Button: tea.MouseLeft})
			case "removed":
				delete(root.chatPages, "profile")
				root.Update(tea.MouseMotionMsg{X: -1, Y: -1, Button: tea.MouseLeft})
			}
			require.False(t, page.IsMessagesScrollbarDragging())
			require.NotNil(t, root.messagesScrollbar, "canceled capture consumes its eventual release")
			require.True(t, root.messagesScrollbar.canceled)
			root.Update(tea.MouseReleaseMsg{X: 4, Y: root.editorTop(), Button: tea.MouseLeft})
			require.Nil(t, root.messagesScrollbar)
			require.Zero(t, page.releases, "cancellation must not replay release into the stale owner")
			require.False(t, root.isDragging)
			require.False(t, root.tabBar.HasPointerCapture())
			if reason == "modal" {
				root.Update(dialog.HideDialogMsg{})
				for range 100 {
					if !root.dialogMgr.Open() {
						break
					}
					scheduler.step(t, root)
				}
				require.False(t, root.dialogMgr.Open(), "subsequent editor click waits for the real modal fade")
			}
			if reason == "blur" {
				root.Update(tea.FocusMsg{})
			}
			root.focusedPanel = PanelContent
			root.Update(tea.MouseClickMsg{X: 4, Y: root.editorTop() + root.editorFrame().GetPaddingTop(), Button: tea.MouseLeft})
			require.Equal(t, PanelEditor, root.focusedPanel)
			require.Nil(t, root.messagesScrollbar)
		})
	}
}

func TestRootMessagesScrollbarCaptureClampsAndRestarts(t *testing.T) {
	for _, lean := range []bool{false, true} {
		root, page := scrollbarCaptureRoot(t, lean, false)
		startRootScrollbarCapture(t, root, page)
		root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseRight})
		require.True(t, page.IsMessagesScrollbarDragging())
		require.NotNil(t, root.messagesScrollbar)
		require.Zero(t, page.releases)
		views := make([]string, 0, 2)
		for _, y := range []int{-1000, 1000} {
			root.Update(tea.MouseMotionMsg{X: -100, Y: y, Button: tea.MouseLeft})
			boundary := page.TranscriptView()
			root.Update(tea.MouseMotionMsg{X: -200, Y: y * 2, Button: tea.MouseLeft})
			require.Equal(t, boundary, page.TranscriptView(), "continued outside motion clamps at the same real transcript boundary")
			views = append(views, boundary)
		}
		require.NotEqual(t, views[0], views[1], "top/bottom drags must actually change the visible transcript")
		root.Update(tea.MouseReleaseMsg{X: -100, Y: 2000, Button: tea.MouseLeft})
		require.False(t, page.IsMessagesScrollbarDragging())
		startRootScrollbarCapture(t, root, page)
		root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
		require.Equal(t, 2, page.releases, "new thumb press starts a fresh capture after release")
		startRootScrollbarCapture(t, root, page)
		root.Update(tea.BlurMsg{})
		require.True(t, root.messagesScrollbar.canceled)
		root.Update(tea.FocusMsg{})
		// A fresh press replaces a canceled tombstone even if no release was
		// reported while the terminal was blurred.
		startRootScrollbarCapture(t, root, page)
		require.False(t, root.messagesScrollbar.canceled)
		root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
		require.Equal(t, 3, page.releases)
	}
}

func TestRootMessagesScrollbarCaptureStartsOnUnfocusedPane(t *testing.T) {
	root, first := scrollbarCaptureRoot(t, false, true)
	page := root.chatPages["second"]
	second := &scrollbarCapturePage{
		Page: page, SplitPresentation: page.(chat.SplitPresentation), messagesScrollbarOwner: page.(messagesScrollbarOwner),
		SplitSidebarPresentation: page.(chat.SplitSidebarPresentation), PresentationVisibility: page.(chat.PresentationVisibility),
		PresentationSelection: page.(chat.PresentationSelection),
	}
	root.chatPages["second"] = second
	// Keep composer geometry stable during this focus change: this test
	// isolates canonical pointer ownership, not a terminal/layout resize.
	root.editors["second"] = &composerInlineBanner{
		Editor: root.editors["second"], ViewportLayout: root.editors["second"].(editor.ViewportLayout), limit: 4,
	}
	root.editors["second"].SetValue("UNCHANGED DRAFT")
	second.Update(messages.WheelCoalescedMsg{Delta: 1_000_000, X: root.paneGeometry.Panes["second"].X, Y: root.paneGeometry.Panes["second"].Y})
	root.View()
	require.Equal(t, "profile", root.paneFocus())
	r := root.paneGeometry.Panes["second"]
	root.Update(tea.MouseClickMsg{X: r.X + r.W - 1, Y: r.Y + r.H - root.paneHeaderHeight() - 1, Button: tea.MouseLeft})
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, r, root.paneGeometry.Panes["second"], "focus alone retains the painted thumb geometry")
	require.True(t, second.IsMessagesScrollbarDragging())
	require.NotNil(t, root.messagesScrollbar)
	require.Same(t, second, root.messagesScrollbar.page)
	require.Equal(t, "second", root.messagesScrollbar.sessionID)
	other := root.paneGeometry.Panes["profile"]
	root.Update(tea.MouseMotionMsg{X: other.X + other.W/2, Y: other.Y + other.H/2, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: other.X + other.W/2, Y: other.Y + other.H/2, Button: tea.MouseLeft})
	require.Equal(t, 1, second.releases)
	require.Zero(t, first.releases)
	require.False(t, second.IsMessagesScrollbarDragging())
	require.Equal(t, "second", root.paneFocus())
}

func TestRootMessagesScrollbarUnfocusedPressUsesPaintedGeometry(t *testing.T) {
	for _, thumb := range []bool{false, true} {
		root, first := scrollbarCaptureRoot(t, false, true)
		page := root.chatPages["second"]
		second := &scrollbarCapturePage{
			Page: page, SplitPresentation: page.(chat.SplitPresentation), messagesScrollbarOwner: page.(messagesScrollbarOwner),
			SplitSidebarPresentation: page.(chat.SplitSidebarPresentation), PresentationVisibility: page.(chat.PresentationVisibility),
			PresentationSelection: page.(chat.PresentationSelection),
		}
		root.chatPages["second"] = second
		// Unlike the stable-geometry case, this destination has no attachment
		// banner. Focusing it grows the transcript after the initial press.
		root.editors["second"].SetValue("destination draft")
		r := root.paneGeometry.Panes["second"]
		second.Update(messages.WheelCoalescedMsg{Delta: 1_000_000, X: r.X, Y: r.Y})
		root.View()
		press := tea.MouseClickMsg{X: r.X + r.W - 1, Y: r.Y + r.H - root.paneHeaderHeight() - 1, Button: tea.MouseLeft}
		if !thumb {
			press.X = r.X + r.W/2
		}
		root.Update(press)
		require.Equal(t, "second", root.paneFocus())
		require.NotEqual(t, r, root.paneGeometry.Panes["second"], "fixture must really change geometry during focus")
		require.Equal(t, 1, second.presses, "original page receives exactly one press")
		require.Equal(t, press, second.pressedAt)
		require.Zero(t, first.presses)
		require.Equal(t, thumb, second.pressedDragging, "only the painted thumb grants capture, before focus relayout")
		if thumb {
			require.NotNil(t, root.messagesScrollbar)
			require.True(t, root.messagesScrollbar.canceled, "actual geometry change safely cancels rather than rebasing stale drag offsets")
			require.False(t, second.IsMessagesScrollbarDragging())
			root.Update(tea.MouseReleaseMsg{X: 4, Y: root.editorTop(), Button: tea.MouseLeft})
			require.Nil(t, root.messagesScrollbar)
			require.Zero(t, second.releases, "canceled release cannot click through into the destination")
		} else {
			require.Nil(t, root.messagesScrollbar, "ordinary content press must not acquire scrollbar capture")
			root.Update(tea.MouseReleaseMsg{X: press.X, Y: press.Y, Button: tea.MouseLeft})
			require.Equal(t, 1, second.releases, "normal content release retains its selection route")
		}
		require.Equal(t, "destination draft", root.editor.Value())
		require.Zero(t, first.releases)
	}
}
