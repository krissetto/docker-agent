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

func TestSetContextUsageRetargetsFromDisplayedValue(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)
	m.SetWidth(40)
	m.SetContextUsageDirect(20, 100)

	require.NotNil(t, m.SetContextUsage(80, 100))
	m.contextPercent = 0.45 // deterministic in-flight displayed value
	require.NotNil(t, m.SetContextUsage(10, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	assert.InDelta(t, 0.10, m.animTo, 0.001)
	assert.Contains(t, m.View(), "Context 45%")

	// Repeating the current target does not restart from a new origin.
	assert.Nil(t, m.SetContextUsage(10, 100))
	assert.InDelta(t, 0.45, m.animFrom, 0.001)
	m.Cancel()
}

func TestContextUsageAnimationRegistersAndCancels(t *testing.T) {
	runtime := animation.NewRuntime()
	m := New(runtime)

	cmd := m.SetContextUsage(50, 100)
	require.NotNil(t, cmd)
	assert.Equal(t, int32(1), runtime.ActiveCount())
	assert.True(t, m.animating)

	m.Cancel()
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

func TestViewNeverExceedsRequestedWidth(t *testing.T) {
	for _, width := range []int{1, 5, 10, 11, 12} {
		t.Run(fmt.Sprintf("width %d", width), func(t *testing.T) {
			m := New(animation.NewRuntime())
			m.SetWidth(width)
			m.SetContextUsageDirect(100, 100)

			view := m.View()
			assert.Equal(t, 1, lipgloss.Height(view))
			assert.LessOrEqual(t, lipgloss.Width(view), width)
			assert.NotContains(t, view, "\n")
		})
	}
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
