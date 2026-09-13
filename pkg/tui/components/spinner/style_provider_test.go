package spinner

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestExplicitStyleProviderPreservesRoleAcrossCoincidentColorsAndReset(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme := styles.DefaultTheme()
	theme.Colors.Accent = "#123456"
	theme.Colors.Highlight = "#123456"
	styles.ApplyTheme(theme)
	require.Equal(t, styles.SpinnerDotsAccentStyle, styles.SpinnerDotsHighlightStyle)
	runtime := animation.NewRuntime()
	accent := NewWithStyleProvider(runtime, ModeSpinnerOnly, func() lipgloss.Style { return styles.SpinnerDotsAccentStyle })
	highlight := NewWithStyleProvider(runtime, ModeSpinnerOnly, func() lipgloss.Style { return styles.SpinnerDotsHighlightStyle })
	highlight = highlight.Reset()
	accent.View()
	highlight.View()
	for _, ref := range []string{"default-light", "default", "nord"} {
		next, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(next)
		require.NotEqual(t, styles.SpinnerDotsAccentStyle, styles.SpinnerDotsHighlightStyle)
		assert.Equal(t, styles.SpinnerDotsHighlightStyle.Render(highlight.RawFrame()), highlight.View(), ref)
		assert.Equal(t, styles.SpinnerDotsAccentStyle.Render(accent.RawFrame()), accent.View(), ref)
		assert.NotEqual(t, accent.View(), highlight.View(), ref)
		highlight = highlight.Reset()
		assert.Equal(t, styles.SpinnerDotsHighlightStyle.Render(highlight.RawFrame()), highlight.View(), ref)
	}
	assert.Zero(t, runtime.ActiveCount(), "styling alone never starts a timer")
	accent.Stop()
	highlight.Stop()
}
