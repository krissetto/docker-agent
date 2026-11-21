package contextbar

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestHoldKeepsDisplayedFillAsPendingOrigin(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	m.SetWidth(40)
	m.SetContextUsageDirect(20, 100)
	require.NotNil(t, m.SetContextUsage(80, 100))
	m.contextPercent = 0.45 // deterministic in-flight displayed value

	m.Hold()
	assert.False(t, m.animating)
	assert.Zero(t, runtime.ActiveCount())
	assert.InDelta(t, 0.45, m.contextPercent, 0.001)
	assert.Contains(t, m.View(), "Context —")

	require.NotNil(t, m.SetContextUsage(0, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	assert.InDelta(t, 0, m.animTo, 0.001)
	assert.Contains(t, m.View(), "Context 0%")
	m.Cancel()
}

func TestSetContextUsageRetargetsFromDisplayedValue(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	m.SetWidth(40)
	m.SetContextUsageDirect(20, 100)

	require.NotNil(t, m.SetContextUsage(80, 100))
	m.contextPercent = 0.45 // deterministic in-flight displayed value
	require.Nil(t, m.SetContextUsage(10, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	assert.InDelta(t, 0.10, m.animTo, 0.001)
	assert.Contains(t, m.View(), "Context 10%")

	// Repeating the current target does not restart from a new origin.
	assert.Nil(t, m.SetContextUsage(10, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	m.Cancel()
}

func TestSetContextUsageWithoutLimitEasesToEmpty(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	m.SetWidth(40)
	m.SetContextUsageDirect(45, 100)

	require.NotNil(t, m.SetContextUsage(0, 0))
	assert.InDelta(t, 0.45, m.contextPercent, 0.001)
	assert.InDelta(t, 0, m.animTo, 0.001)
	assert.Contains(t, m.View(), "Context —")
	m.Cancel()

	// An already empty track has nothing to animate.
	m.SetContextUsageDirect(0, 0)
	assert.Nil(t, m.SetContextUsage(0, 0))
	assert.Zero(t, runtime.ActiveCount())
}

func TestRetargetBeforeFirstFrameKeepsTinyVisibleFill(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	m.SetContextUsageDirect(1, 4000) // Below 0.1%, but still one visible cell.
	require.NotNil(t, m.SetContextUsage(0, 4000))
	require.Nil(t, m.SetContextUsage(1, 4000)) // Switch back before the first tick.
	require.Zero(t, runtime.ActiveCount())
	require.InDelta(t, 1.0/4000, m.contextPercent, 1e-10)
}

func TestContextUsageAnimationRegistersAndCancels(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)

	cmd := m.SetContextUsage(50, 100)
	require.NotNil(t, cmd)
	assert.Equal(t, int32(1), runtime.ActiveCount())
	assert.True(t, m.animating)
	tick, accepted := runtime.Accept(cmd().(animation.TickMsg))
	require.True(t, accepted)
	m.Update(tick)
	assert.Positive(t, m.contextPercent, "owner continuation advances displayed usage")
	queued := runtime.Continue()
	require.NotNil(t, queued)

	m.Cancel()
	_, accepted = runtime.Accept(queued().(animation.TickMsg))
	assert.False(t, accepted)
	assert.Nil(t, runtime.Continue())
	assert.False(t, m.animating)
	assert.Equal(t, int32(0), runtime.ActiveCount())
}

func TestDirectUpdateAndCancelAreSafe(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)

	m.SetContextUsageDirect(50, 100)
	assert.InDelta(t, 0.5, m.contextPercent, 0.001)
	assert.Zero(t, runtime.ActiveCount())

	_ = m.SetContextUsage(75, 100)
	require.Equal(t, int32(1), runtime.ActiveCount())
	m.SetContextUsageDirect(75, 100)
	assert.True(t, m.animating, "same target preserves an intentional animation")
	m.Cancel()
	m.Cancel()
	assert.False(t, m.animating)
	assert.Zero(t, runtime.ActiveCount())

	m.SetContextUsageDirect(-10, 100)
	assert.Zero(t, m.contextPercent)
	m.SetContextUsageDirect(200, 100)
	assert.InDelta(t, 1.0, m.contextPercent, 0)
}

func TestDirectUpdateCancelsDifferentTarget(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	_ = m.SetContextUsage(25, 100)

	m.SetContextUsageDirect(80, 100)
	assert.InDelta(t, 0.8, m.contextPercent, 0.001)
	assert.False(t, m.animating)
	assert.Zero(t, runtime.ActiveCount())
}

func TestViewIsExactlyOneRowAtRequestedWidth(t *testing.T) {
	m := New(animation.NewRuntime())
	m.SetWidth(32)
	m.SetContextUsageDirect(70, 100)

	view := m.View()
	assert.Equal(t, 1, lipgloss.Height(view))
	assert.Equal(t, 32, lipgloss.Width(view))
	assert.Contains(t, view, "Context 70%")
	assert.NotContains(t, view, "\n")
	assert.Equal(t, 20, strings.Count(view, blockFill))
}

func TestViewFitsContextLabelWithOneSpaceAfterTrack(t *testing.T) {
	m := New(animation.NewRuntime())
	for _, tt := range []struct {
		used  int64
		label string
	}{
		{5, " Context 5%"},
		{50, " Context 50%"},
		{100, " Context 100%"},
	} {
		m.SetContextUsageDirect(tt.used, 100)
		for _, width := range []int{1, 10, 11, 12, 13, 14, 36} {
			m.SetWidth(width)
			label := tt.label
			if len(label) > width {
				label = ""
			}
			want := strings.Repeat(blockFill, width-len(label)) + label
			assert.Equal(t, want, ansi.Strip(m.View()), "usage %d at width %d", tt.used, width)
		}
	}
}

func TestViewReadingChangesKeepFill(t *testing.T) {
	m := New(animation.NewRuntime())
	m.SetWidth(40)
	track, _, _ := strings.Cut(lipgloss.NewStyle().Background(styles.EditorBg).Foreground(styles.Background).Render(blockFill), blockFill)
	fill := func() string {
		view := m.View()
		before, _, _ := strings.Cut(view, track)
		return before
	}
	m.SetContextUsageDirect(42, 100)
	held := fill()
	require.Equal(t, 11, strings.Count(held, blockFill))

	// A pending reading is shorter, yet still ends flush and keeps the fill.
	m.Hold()
	assert.Equal(t, strings.Repeat(blockFill, 30)+" Context —", ansi.Strip(m.View()))
	assert.Equal(t, held, fill())
}

func TestPositiveUsageAlwaysFillsLeadingCell(t *testing.T) {
	m := New(animation.NewRuntime())
	m.SetWidth(20)
	m.SetContextUsageDirect(1, 1000)
	positive := m.View()

	m.SetContextUsageDirect(0, 1000)
	zero := m.View()
	require.Equal(t, lipgloss.Width(zero), lipgloss.Width(positive))
	first, _ := utf8.DecodeRuneInString(ansi.Strip(positive))
	assert.Equal(t, blockFill, string(first))
	assert.NotEqual(t, strings.SplitN(zero, blockFill, 2)[0], strings.SplitN(positive, blockFill, 2)[0],
		"the leading cell must use fill ANSI rather than track ANSI")
}

func TestFillColorTransitionsInfoWarningError(t *testing.T) {
	assert.Equal(t, styles.Info, fillColor(0.2))
	assert.Equal(t, styles.Warning, fillColor(criticalThreshold))
	assert.Equal(t, styles.Error, fillColor(1))
}

func TestAnimationDurationSupportsRootShrinkTiming(t *testing.T) {
	// Root integration should run its editor shrink separately at roughly
	// 330ms; the usage bar remains a slower, readable transition.
	assert.Equal(t, 670*time.Millisecond, animDuration)
}

func TestContextTrackUsesEditorSurfaceNotTabSurface(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme, err := styles.LoadTheme("default-light")
	require.NoError(t, err)
	theme.Colors.EditorBg = "#312345"
	require.NotEqual(t, theme.Colors.TabBg, theme.Colors.EditorBg)
	styles.ApplyTheme(theme)
	m := New(animation.NewRuntime())
	m.SetWidth(40)
	m.SetContextUsageDirect(1, 100)
	view := m.View()
	r, g, b, _ := styles.EditorBg.RGBA()
	background := fmt.Sprintf("48;2;%d;%d;%dm", r>>8, g>>8, b>>8)
	require.Contains(t, view, background)
	require.Equal(t, 40, ansi.StringWidth(view))
	require.Contains(t, view, " Context 1%")
}

func TestWarmThemeSwitchPreservesHalfCellTrackAndLabelCutout(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New(animation.NewRuntime())
	m.SetWidth(48)
	for _, usage := range []int64{0, 1, 50, 95, 100} {
		m.SetContextUsageDirect(usage, 100)
		before := ansi.Strip(m.View())
		for _, ref := range []string{"default-light", "nord", "default"} {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)
			warm := m.View()
			fresh := New(animation.NewRuntime())
			fresh.SetWidth(48)
			fresh.SetContextUsageDirect(usage, 100)
			assert.Equal(t, fresh.View(), warm)
			assert.Equal(t, before, ansi.Strip(warm))
			assert.Equal(t, 48, lipgloss.Width(warm))
			assert.Contains(t, warm, lipgloss.NewStyle().Foreground(styles.TextMuted).Render(fmt.Sprintf(" Context %d%%", usage)))
			if usage == 0 {
				assert.Contains(t, warm, lipgloss.NewStyle().Background(styles.EditorBg).Foreground(styles.Background).Render(strings.Repeat(blockFill, 37)))
			}
		}
	}
}
