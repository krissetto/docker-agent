package editor

import (
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func assertColorEqual(t *testing.T, want, got color.Color, msgAndArgs ...any) {
	t.Helper()
	wantR, wantG, wantB := styles.ColorToRGB(want)
	gotR, gotG, gotB := styles.ColorToRGB(got)
	assert.InDelta(t, wantR, gotR, 0.01, msgAndArgs...)
	assert.InDelta(t, wantG, gotG, 0.01, msgAndArgs...)
	assert.InDelta(t, wantB, gotB, 0.01, msgAndArgs...)
}

// TestThemeChanged_RestylesSearchInput guards against the historySearch input
// silently keeping stale colors after a live theme switch, since its styles
// are otherwise only set once in New.
func TestThemeChanged_RestylesSearchInput(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide style variables.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	tmpDir := t.TempDir()
	h, err := history.New(tmpDir)
	require.NoError(t, err)

	e := New(h).(*editor)

	theme := styles.DefaultTheme()
	theme.Ref = "theme-changed-test"
	theme.Colors.TextMuted = "#123456"
	theme.Colors.Placeholder = "#abcdef"
	styles.ApplyTheme(theme)

	m, _ := e.Update(messages.ThemeChangedMsg{})
	e = m.(*editor)

	wantMuted := styles.MutedStyle.GetForeground()
	searchStyles := e.searchInput.Styles()
	assertColorEqual(t, wantMuted, searchStyles.Focused.Text.GetForeground(),
		"searchInput focused text should follow the new theme's muted color")
	assertColorEqual(t, wantMuted, searchStyles.Focused.Placeholder.GetForeground(),
		"searchInput focused placeholder should follow the new theme's muted color")
	assertColorEqual(t, wantMuted, searchStyles.Blurred.Text.GetForeground(),
		"searchInput blurred text should follow the new theme's muted color")
	assertColorEqual(t, wantMuted, searchStyles.Blurred.Placeholder.GetForeground(),
		"searchInput blurred placeholder should follow the new theme's muted color")

	// The main textarea keeps getting restyled too, so the fix doesn't
	// regress the pre-existing behavior.
	assertColorEqual(t, styles.PlaceholderColor, e.textarea.Styles().Focused.Placeholder.GetForeground(),
		"textarea placeholder should follow the new theme's placeholder color")
}

func TestWarmEditorThemeSwitchWithoutNotification(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	e := New(nil).(*editor)
	e.SetSize(36, 3)
	e.SetValue("kept draft")
	e.Blur()
	e.View()
	before := e.textarea.Layout()
	for _, ref := range []string{"default-light", "nord", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		got := e.View()
		assert.Equal(t, before, e.textarea.Layout(), "theme refresh changes presentation, not editing geometry")
		fresh := New(nil).(*editor)
		fresh.SetSize(36, 3)
		fresh.SetValue("kept draft")
		fresh.Blur()
		assert.Equal(t, fresh.View(), got, ref)
		assert.Equal(t, "kept draft", e.Value())
		assertColorEqual(t, styles.PlaceholderColor, e.textarea.Styles().Focused.Placeholder.GetForeground())
		assertColorEqual(t, styles.MutedStyle.GetForeground(), e.searchInput.Styles().Focused.Text.GetForeground())
	}
}
