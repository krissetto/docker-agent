package tui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestRenderResizeHandle_TinyWidths guards against a panic (negative
// strings.Repeat count) when the terminal reports degenerate sizes such as
// 0x0 or 1x1: the padded inner width goes negative for widths smaller than
// the app's horizontal padding.
func TestRenderResizeHandle_TinyWidths(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)

	for _, width := range []int{-1, 0, 1, appPaddingHorizontal, appPaddingHorizontal + 1, 80} {
		out := m.renderResizeHandle(width)
		if width <= appPaddingHorizontal {
			assert.Empty(t, out)
		} else {
			assert.NotEmpty(t, out)
		}
	}
}

// TestRenderResizeHandle_SuffixNeverOverflows pins that a status suffix wider
// than the handle line is truncated instead of overflowing the row on narrow
// terminals.
func TestRenderResizeHandle_SuffixNeverOverflows(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.sessionState = &service.SessionState{}
	m.sessionState.SetPauseState(service.PausePaused)

	for _, width := range []int{5, 10, 20, 80, 200} {
		out := m.renderResizeHandle(width)
		assert.LessOrEqual(t, lipgloss.Width(out), width, "width %d", width)
	}
}

func TestLineWithSuffix(t *testing.T) {
	t.Parallel()

	// The suffix fits: the line is truncated to make room.
	out := lineWithSuffix("──────────", " ok", 8)
	assert.Equal(t, "───── ok", out)
	assert.Equal(t, 8, lipgloss.Width(out))

	// The suffix alone is too wide: it is truncated and the line dropped.
	out = lineWithSuffix("──────────", " a very long status", 8)
	assert.Equal(t, 8, lipgloss.Width(out))
}

func TestRootWorkingSpinnerRetainsHighlightRoleAcrossThemeAndTabSwitch(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	collision := *original
	collision.Colors.Accent = "#123456"
	collision.Colors.Highlight = "#123456"
	styles.ApplyTheme(&collision)
	sess := session.New(session.WithID("spinner-role-session"))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	root.sessionState.SetPauseState(service.PausePausing)
	changed := collision
	changed.Colors.Accent = "#ff3344"
	changed.Colors.Highlight = "#33ccaa"
	styles.ApplyTheme(&changed)
	assertRole := func() {
		t.Helper()
		wanted := color.NRGBAModel.Convert(styles.SpinnerDotsHighlightStyle.GetForeground())
		wrong := color.NRGBAModel.Convert(styles.SpinnerDotsAccentStyle.GetForeground())
		require.NotEqual(t, wanted, wrong)
		for _, cell := range chromeCells(root.workingSpinner.View()) {
			require.Equal(t, wanted, color.NRGBAModel.Convert(cell.fg))
		}
		require.NotEmpty(t, root.renderResizeHandle(root.width), "the real separator remains while activity lives in the pane header")
		require.NotContains(t, ansi.Strip(root.renderResizeHandle(root.width)), "Working")
		require.Contains(t, root.paneActivity(root.paneFocus()), "pausing")
	}
	assertRole()
	// Exercise the second production constructor under the same initially colliding roles.
	styles.ApplyTheme(&collision)
	second := session.New(session.WithID("spinner-role-next-session"))
	secondApp := app.New(t.Context(), nil, second, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := root.supervisor.AddSession(t.Context(), secondApp, second, "", nil)
	require.NoError(t, err)
	root.handleSwitchTab(second.ID)
	root.sessionState.SetPauseState(service.PausePausing)
	styles.ApplyTheme(&changed)
	assertRole()
}

func TestSeparatorResizeHoverOcclusionAndCancellation(t *testing.T) {
	root := splitTestRoot(t)
	x, y := tabFrameOrigin()+5, root.contentHeight
	require.True(t, root.composerResizeHit(x, y))
	require.False(t, root.composerResizeHit(x, root.editorTop()+styles.EditorStyle.GetPaddingTop()), "editable text is never a resize target")
	require.False(t, root.composerResizeHit(x, root.contentHeight+1), "tab row keeps tab gestures")
	root.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, root.isHoveringHandle)
	require.NotEmpty(t, root.renderResizeHandle(root.width))
	root.Update(tea.MouseMotionMsg{X: -1, Y: -1})
	require.False(t, root.isHoveringHandle)
	for _, cancel := range []string{"release outside", "blur", "modal"} {
		root.Update(tea.MouseClickMsg{X: x, Y: root.contentHeight, Button: tea.MouseLeft})
		require.True(t, root.isDragging)
		root.Update(tea.MouseMotionMsg{X: x, Y: root.contentHeight - 4, Button: tea.MouseLeft})
		require.False(t, root.editorHeightMotion.Running(), "direct allocation has no animation lease")
		switch cancel {
		case "release outside":
			root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
		case "blur":
			root.Update(tea.BlurMsg{})
		case "modal":
			root.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "resize-occlusion"}})
			require.False(t, root.composerResizeHit(x, root.contentHeight))
		}
		require.False(t, root.isDragging)
		require.False(t, root.isHoveringHandle)
	}
	root.updateDialogCmd(dialog.HideDialogMsg{})
	root.handleWindowResize(1, 1)
	require.False(t, root.composerResizeHit(-1, root.contentHeight))
	require.NotPanics(t, func() { root.composerView() })
}

func TestComposerDefaultEqualsReachableDragMinimumAndNoMoveIsStable(t *testing.T) {
	for _, split := range []bool{false, true} {
		root := splitTestRoot(t)
		if split {
			root.splitPane("second", "profile", splitRight)
		}
		root.editor.SetValue("")
		root.resizeAll()
		initial := root.editorHeight
		require.Equal(t, 1, initial, "keep the compact default; fix geometry instead of inflating it")
		x, y := tabFrameOrigin()+3, root.contentHeight
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		root.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
		require.Equal(t, initial, root.editorHeight)
		root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		require.Equal(t, initial, root.editorHeight, "no-move pickup/release does not resize")
		root.Update(tea.MouseClickMsg{X: x, Y: root.contentHeight, Button: tea.MouseLeft})
		root.Update(tea.MouseMotionMsg{X: x, Y: root.contentHeight - 5, Button: tea.MouseLeft})
		require.Greater(t, root.editorHeight, initial)
		root.Update(tea.MouseMotionMsg{X: x, Y: root.height - 1, Button: tea.MouseLeft})
		require.Equal(t, initial, root.editorHeight, "manual minimum is the real one-row textarea default")
		root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
		require.False(t, root.editorHeightMotion.Running())
		require.Zero(t, root.ar.ActiveCount())
	}
}

func TestComposerResizeGeometryNormalNarrowTinyKeepsTextPadding(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {48, 20}, {24, 12}, {1, 1}} {
		root := splitTestRoot(t)
		root.handleWindowResize(size[0], size[1])
		before := root.editorHeight
		if root.contentHeight < root.height && root.composerResizeHit(tabFrameOrigin(), root.contentHeight) {
			x, y := tabFrameOrigin(), root.contentHeight
			root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			root.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
			root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Equal(t, before, root.editorHeight)
		}
		frame := root.View().Content
		require.Equal(t, size[1], lipgloss.Height(frame))
		require.LessOrEqual(t, lipgloss.Width(frame), size[0])
		if size[0] >= 24 && size[1] >= 12 {
			require.Contains(t, ansi.Strip(root.renderResizeHandle(root.width)), "─", "the restored separator carries the visible resize grip")
			require.Positive(t, styles.EditorStyle.GetPaddingTop())
			require.False(t, root.composerResizeHit(tabFrameOrigin(), root.editorTop()+styles.EditorStyle.GetPaddingTop()), "first text row is never a resize hit")
		}
	}
}

func TestRestoredSeparatorMatchesPriorGripWithoutStatusClutter(t *testing.T) {
	root := splitTestRoot(t)
	for _, width := range []int{24, 64, 120} {
		root.handleWindowResize(width, 40)
		separator := root.renderResizeHandle(width)
		require.Equal(t, width, ansi.StringWidth(separator))
		require.Equal(t, " "+strings.Repeat("─", width-2)+" ", ansi.Strip(separator))
		root.sessionState.SetPauseState(service.PausePaused)
		require.Equal(t, separator, root.renderResizeHandle(width), "status never replaces the restored grip")
		require.Equal(t, regionResizeHandle, root.hitTestRegion(root.contentHeight))
		require.Equal(t, regionTabBar, root.hitTestRegion(root.contentHeight+1))
		require.False(t, root.composerResizeHit(4, root.editorTop()), "rejected padding-only handler is removed")
		rows := strings.Split(root.View().Content, "\n")
		require.Equal(t, ansi.Strip(separator), ansi.Strip(rows[root.contentHeight]))
		require.NotContains(t, ansi.Strip(rows[root.contentHeight]), "Working")
		require.NotContains(t, ansi.Strip(rows[root.contentHeight]), "Esc")
	}
}
