package tui

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
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
			assert.Empty(t, out, "width %d", width)
		} else {
			assert.NotEmpty(t, out, "width %d", width)
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
		require.Contains(t, root.renderResizeHandle(root.width), root.workingSpinner.View(), "root indicator renders the explicitly bound working role")
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
