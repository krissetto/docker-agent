package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// composerInlineBanner keeps all textarea behavior real and supplies synthetic
// attachment presentation/preview bytes without any filesystem attachment.
type composerInlineBanner struct {
	editor.Editor
	editor.ViewportLayout

	limit, previewY int
	expanded        bool
}

func (e *composerInlineBanner) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	updated, cmd := e.Editor.Update(msg)
	e.Editor = updated.(editor.Editor)
	return e, cmd
}

func (e *composerInlineBanner) SetBannerMaxHeight(height int) { e.limit = height }
func (e *composerInlineBanner) BannerHeight() int {
	rows := 3
	if e.expanded {
		rows = 4
	}
	return min(rows, e.limit)
}

func (e *composerInlineBanner) BannerView(width int) string {
	if e.BannerHeight() == 0 {
		return ""
	}
	rows := []string{"", "attachment border", "INLINE-ATTACHMENT", "INLINE-PREVIEW"}
	for i := range rows {
		rows[i] = lipgloss.NewStyle().Width(width).Render(ansi.Truncate(rows[i], width, ""))
	}
	return strings.Join(rows[:e.BannerHeight()], "\n")
}
func (e *composerInlineBanner) ToggleContextBar() { e.expanded = !e.expanded }
func (e *composerInlineBanner) AttachmentAtPosition(_, y int) (editor.AttachmentPreview, bool) {
	e.previewY = y
	return editor.AttachmentPreview{Title: "inline", Content: "SYNTHETIC PREVIEW"}, e.expanded && y == 3
}

func installComposerInlineBanner(root *appModel) *composerInlineBanner {
	e := &composerInlineBanner{Editor: root.editor, ViewportLayout: root.editor.(editor.ViewportLayout), limit: 4, previewY: -1}
	root.editor = e
	root.resizeAll()
	root.viewCacheValid = false
	return e
}

func TestComposerLayoutAutomaticGrowthBeyondOldCeiling(t *testing.T) {
	for _, lean := range []bool{false, true} {
		root := splitTestRoot(t)
		root.leanMode = lean
		root.resizeAll()
		root.editor.Focus()
		root.focusedPanel = PanelEditor
		root.Update(tea.PasteMsg{Content: "FIRST 界 cafe\u0301 👩‍💻"})
		maximum := root.composerMaxTextHeight()
		require.Greater(t, maximum, 3)
		for rows := 2; rows <= maximum+3; rows++ {
			root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
			require.Equal(t, min(rows, maximum), root.editorHeight)
			require.False(t, root.editorHeightMotion.Running())
			frame := ansi.Strip(root.View().Content)
			if rows <= maximum {
				require.Contains(t, frame, "FIRST 界 cafe\u0301 👩‍💻", "first line is visible in the newline's frame, before the next key")
			} else {
				require.NotContains(t, ansi.Strip(root.editor.View()), "FIRST", "overflow scrolls instead of exceeding the dynamic allocation")
			}
			root.Update(tea.PasteMsg{Content: "next"})
		}
		require.Zero(t, root.manualEditorHeight)
	}
}

func TestComposerLayoutManualHeightIsFixedAndRestoresAfterClamp(t *testing.T) {
	root := splitTestRoot(t)
	root.editor.SetValue("one\ntwo\nthree\nfour\nfive\nsix")
	root.resizeAll()
	require.Equal(t, 6, root.editorHeight)
	x, y := 4, root.composerLayout().separatorTop
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: x, Y: y + 4, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: x, Y: y + 4, Button: tea.MouseLeft})
	require.Equal(t, 2, root.manualEditorHeight)
	require.Equal(t, 2, root.editorHeight, "manual choice can be smaller than content")
	root.editor.InsertText("\nseven\neight")
	root.resizeAll()
	require.Equal(t, 2, root.editorHeight)
	root.handleEditorResize(root.composerLayout().separatorTop - 8)
	require.Equal(t, 10, root.manualEditorHeight)
	root.editor.SetValue("short")
	root.resizeAll()
	require.Equal(t, 10, root.editorHeight, "manual choice can be larger than content")
	root.handleWindowResize(40, 12)
	require.Equal(t, root.composerMaxTextHeight(), root.editorHeight)
	require.Equal(t, 10, root.manualEditorHeight)
	root.handleWindowResize(120, 40)
	require.Equal(t, 10, root.editorHeight, "temporary viewport clamp does not erase the user's choice")
	require.False(t, root.editorHeightMotion.Running())
}

func TestComposerLayoutNoMotionKeepsAutomaticAndPendingShrink(t *testing.T) {
	root := splitTestRoot(t)
	root.editor.SetValue("one\ntwo\nthree\nfour\nfive")
	root.resizeAll()
	root.editor.Focus()
	root.focusedPanel = PanelEditor
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Empty(t, root.editor.Value())
	require.Zero(t, root.manualEditorHeight)
	require.True(t, root.editorShrinkDelayed)
	require.True(t, root.editorHeightMotion.Running())
	require.Equal(t, editorShrinkDelay+animation.ShortDuration, root.editorHeightMotion.Duration())
	y := root.composerLayout().separatorTop
	root.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: 4, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	require.Zero(t, root.manualEditorHeight, "pickup is not a manual allocation")
	require.True(t, root.editorShrinkDelayed)
	require.True(t, root.editorHeightMotion.Running(), "no-motion pickup does not cancel shared hold/ease")
	root.Update(tea.PasteMsg{Content: "one\ntwo\nthree\nfour\nfive"})
	root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	require.Equal(t, 6, root.editorHeight)
	require.False(t, root.editorHeightMotion.Running(), "growth interrupts the old shrink")
	root.handleEditorResize(root.composerLayout().separatorTop - 2)
	require.Positive(t, root.manualEditorHeight)
	root.handleSwitchTab("second")
	require.Zero(t, root.manualEditorHeight)
	require.Equal(t, 1, root.editorHeightTarget)
	root.handleSwitchTab("profile")
	require.Zero(t, root.manualEditorHeight)
	require.Equal(t, 6, root.editorHeightTarget)
}

func TestComposerLayoutBannerPrecedesRealSeparatorAndRoutesPointer(t *testing.T) {
	root := splitTestRoot(t)
	e := installComposerInlineBanner(root)
	root.editor.SetValue("DRAFT")
	root.resizeAll()
	g := root.composerLayout()
	require.Equal(t, root.contentHeight, g.bannerTop)
	require.Equal(t, g.bannerTop+3, g.separatorTop)
	require.Equal(t, g.separatorTop+1, g.tabsTop)
	frame := strings.Split(root.composeView().Content, "\n")
	require.Contains(t, ansi.Strip(frame[g.bannerTop+2]), "INLINE-ATTACHMENT")
	require.Equal(t, ansi.Strip(root.renderResizeHandle(root.width)), ansi.Strip(frame[g.separatorTop]))
	for y := g.bannerTop; y < g.separatorTop; y++ {
		require.Equal(t, regionContextBar, root.hitTestRegion(y))
		require.False(t, root.composerResizeHit(4, y))
	}
	require.Equal(t, regionContent, root.hitTestRegion(g.bannerTop-1))
	require.Equal(t, regionResizeHandle, root.hitTestRegion(g.separatorTop))
	require.Equal(t, regionTabBar, root.hitTestRegion(g.tabsTop))
	require.Equal(t, regionEditor, root.hitTestRegion(g.editorTop))
	require.Equal(t, regionContextUsage, root.hitTestRegion(g.editorBottom))
	before := root.editorHeight
	root.Update(tea.MouseClickMsg{X: 4, Y: g.separatorTop, Button: tea.MouseLeft})
	require.True(t, root.isDragging)
	root.Update(tea.MouseMotionMsg{X: 4, Y: g.separatorTop, Button: tea.MouseLeft})
	require.Equal(t, before, root.editorHeight, "attachments above the handle do not enter drag subtraction")
	root.Update(tea.MouseMotionMsg{X: 4, Y: g.separatorTop - 3, Button: tea.MouseLeft})
	require.Equal(t, before+3, root.editorHeight)
	root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
	require.False(t, root.isDragging)
	require.False(t, root.editorHeightMotion.Running())
	g = root.composerLayout()
	root.Update(tea.MouseClickMsg{X: 4, Y: g.bannerTop + 2, Button: tea.MouseLeft})
	require.Equal(t, 2, e.previewY, "banner clicks use banner-local coordinates")
	require.True(t, e.expanded)
	require.False(t, root.dialogMgr.Open())
	root.Update(tea.MouseClickMsg{X: 4, Y: root.composerLayout().bannerTop + 3, Button: tea.MouseLeft})
	require.Equal(t, 3, e.previewY)
	require.True(t, root.dialogMgr.Open())
	require.Contains(t, root.dialogMgr.TopDialog().View(), "SYNTHETIC PREVIEW")
	require.False(t, root.composerResizeHit(4, root.composerLayout().separatorTop), "modal occlusion")
	require.Equal(t, "DRAFT", root.editor.Value())
}

func TestComposerLayoutTabsRemainInteractiveBelowAttachments(t *testing.T) {
	root := splitTestRoot(t)
	installComposerInlineBanner(root)
	g := root.composerLayout()
	var x int
	found := false
	for row := range strings.SplitSeq(root.tabBar.View(), "\n") {
		before, _, ok := strings.Cut(ansi.Strip(row), "second")
		if ok {
			x, found = tabFrameOrigin()+ansi.StringWidth(before)+1, true
			break
		}
	}
	require.True(t, found, "second tab has a rendered label")
	root.Update(tea.MouseClickMsg{X: x, Y: g.tabsTop, Button: tea.MouseLeft})
	require.True(t, root.tabBar.HasPointerCapture(), "banner offset is removed from tab press")
	require.False(t, root.isDragging, "tab press does not acquire composer resize")
	root.Update(tea.MouseMotionMsg{X: x + 3, Y: g.tabsTop, Button: tea.MouseLeft})
	require.True(t, root.tabBar.HasFloatingOverlay())
	layer := root.tabBar.GetDragLayerInfo(tabFrameWidth(root.width), g.tabsTop)
	require.NotNil(t, layer)
	require.Equal(t, g.tabsTop, layer.Y)
	root.Update(tea.MouseReleaseMsg{X: x + 3, Y: g.tabsTop, Button: tea.MouseLeft})
	require.False(t, root.tabBar.HasPointerCapture(), "release retains tab-local coordinates")
}

func TestComposerLayoutClippedBannerLeanAndTinyCompletion(t *testing.T) {
	for _, lean := range []bool{false, true} {
		root := splitTestRoot(t)
		root.leanMode = lean
		e := installComposerInlineBanner(root)
		e.expanded = true
		for _, size := range [][2]int{{120, 40}, {24, 12}, {12, 7}, {2, 2}, {1, 1}, {80, 30}} {
			root.handleWindowResize(size[0], size[1])
			g := root.composerLayout()
			require.Equal(t, root.contentHeight, g.bannerTop)
			require.LessOrEqual(t, g.contextBottom+root.messageBarHeight(), root.height)
			for y := range root.height {
				if y >= g.bannerTop && y < g.separatorTop {
					require.Equal(t, regionContextBar, root.hitTestRegion(y))
				} else {
					require.NotEqual(t, regionContextBar, root.hitTestRegion(y), "clipped attachment rows have no hidden hit targets")
				}
			}
			if lean {
				require.Equal(t, g.separatorTop, g.editorTop)
				require.False(t, root.composerResizeHit(0, g.separatorTop))
			}
			root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "reachable tool", Value: "tool"}}})
			root.resizeAll()
			if popup := root.completions.View(); popup != "" {
				require.LessOrEqual(t, lipgloss.Height(popup), root.contentHeight, "popup ends above all composer chrome")
				require.LessOrEqual(t, lipgloss.Width(popup), root.width)
			}
			require.Equal(t, root.height, lipgloss.Height(root.composeView().Content))
		}
		root.updateDialogCmd(dialog.HideDialogMsg{})
		root.err = errors.New("visible error")
		require.Contains(t, ansi.Strip(root.composeView().Content), "visible error")
		require.Equal(t, regionOutside, root.hitTestRegion(root.composerLayout().separatorTop))
		require.False(t, root.composerResizeHit(4, root.composerLayout().separatorTop))
	}
}
