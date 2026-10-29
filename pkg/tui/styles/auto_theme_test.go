package styles

import (
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

// isolateUserConfig points the user config at an empty temp dir and restores
// the previous polarity/enabled state afterwards, so tests can mutate the
// package-level auto-theme state without leaking into each other.
func isolateUserConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	prevDark := TerminalIsDark()
	prevEnabled := AutoThemeEnabled()
	t.Cleanup(func() {
		paths.SetConfigDir("")
		SetTerminalDark(prevDark)
		SetAutoThemeEnabled(prevEnabled)
	})
	return dir
}

func TestTerminalPolarityDefaultsToDark(t *testing.T) {
	isolateUserConfig(t)

	SetTerminalDark(true)
	assert.True(t, TerminalIsDark())

	SetTerminalDark(false)
	assert.False(t, TerminalIsDark())
}

func TestAutoThemeEnabledRoundTrip(t *testing.T) {
	isolateUserConfig(t)

	SetAutoThemeEnabled(true)
	assert.True(t, AutoThemeEnabled())

	SetAutoThemeEnabled(false)
	assert.False(t, AutoThemeEnabled())
}

func TestResolveThemeRefPassesThroughConcreteRefs(t *testing.T) {
	isolateUserConfig(t)

	assert.Equal(t, "nord", ResolveThemeRef("nord"))
	assert.Equal(t, DefaultThemeRef, ResolveThemeRef(DefaultThemeRef))
	assert.Empty(t, ResolveThemeRef(""))
}

func TestResolveThemeRefAutoDefaults(t *testing.T) {
	isolateUserConfig(t)

	SetTerminalDark(true)
	assert.Equal(t, DefaultThemeRef, ResolveThemeRef(AutoThemeRef))

	SetTerminalDark(false)
	assert.Equal(t, DefaultLightThemeRef, ResolveThemeRef(AutoThemeRef))
}

func TestResolveThemeRefAutoHonorsConfiguredPair(t *testing.T) {
	dir := isolateUserConfig(t)
	cfg := "settings:\n  theme: auto\n  theme_dark: tokyo-night\n  theme_light: gruvbox-light\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600))

	SetTerminalDark(true)
	assert.Equal(t, "tokyo-night", ResolveThemeRef(AutoThemeRef))

	SetTerminalDark(false)
	assert.Equal(t, "gruvbox-light", ResolveThemeRef(AutoThemeRef))
}

func TestDefaultLightThemeLoads(t *testing.T) {
	t.Parallel()

	theme, err := LoadTheme(DefaultLightThemeRef)
	require.NoError(t, err)
	assert.Equal(t, "Docker Next - Light", theme.Name)
	assert.True(t, IsBuiltinTheme(DefaultLightThemeRef))

	refs, err := ListThemeRefs()
	require.NoError(t, err)
	assert.Contains(t, refs, DefaultLightThemeRef)
}

func TestUserThemeNamedAutoListsUnderUserPrefix(t *testing.T) {
	dir := t.TempDir()
	paths.SetDataDir(dir)
	t.Cleanup(func() { paths.SetDataDir("") })

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "themes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "themes", "auto.yaml"), []byte("name: Not The Sentinel\n"), 0o600))

	refs, err := ListThemeRefs()
	require.NoError(t, err)
	assert.Contains(t, refs, UserThemePrefix+AutoThemeRef, "a user theme named auto must not shadow the sentinel")
	assert.NotContains(t, refs, AutoThemeRef)
}

// TestDefaultLightThemeContrast checks the readability of the bundled light
// theme: every foreground role must clear a WCAG-ish contrast ratio against
// the background it is rendered on.
func TestDefaultLightThemeContrast(t *testing.T) {
	t.Parallel()

	theme, err := LoadTheme(DefaultLightThemeRef)
	require.NoError(t, err)
	c := theme.Colors

	// Sanity check: this is a light theme.
	bgLum, ok := relativeLuminanceHex(c.Background)
	require.True(t, ok)
	assert.Greater(t, bgLum, 0.5, "default-light background should be light")

	checks := []struct {
		name     string
		fg, bg   string
		minRatio float64
	}{
		{"text_bright on background", c.TextBright, c.Background, 7},
		{"text_primary on background", c.TextPrimary, c.Background, 7},
		{"text_secondary on background", c.TextSecondary, c.Background, 4.5},
		{"text_muted on background", c.TextMuted, c.Background, 4.5},
		{"accent on background", c.Accent, c.Background, 4.5},
		{"accent_muted on background", c.AccentMuted, c.Background, 4.5},
		{"success on background", c.Success, c.Background, 4.5},
		{"error on background", c.Error, c.Background, 4.5},
		{"warning on background", c.Warning, c.Background, 4.5},
		{"info on background", c.Info, c.Background, 4.5},
		{"highlight on background", c.Highlight, c.Background, 4.5},
		{"error_strong on background", c.ErrorStrong, c.Background, 4.5},
		{"text_primary on background_alt", c.TextPrimary, c.BackgroundAlt, 4.5},
		{"text_primary on selected", c.TextPrimary, c.Selected, 4.5},
		{"selected_fg on brand", c.SelectedFg, c.Brand, 4.5},
		{"tab_active_fg on tab_active_bg", c.TabActiveFg, c.TabActiveBg, 4.5},
		{"tab_inactive_fg on tab_bg", c.TabInactiveFg, c.TabBg, 4.5},
		{"placeholder on background", c.Placeholder, c.Background, 4.5},
		{"badge_accent on background", c.BadgeAccent, c.Background, 4.5},
		{"badge_info on background", c.BadgeInfo, c.Background, 4.5},
		{"badge_success on background", c.BadgeSuccess, c.Background, 4.5},
		{"success on diff_add_bg", c.Success, c.DiffAddBg, 4.5},
		{"error on diff_remove_bg", c.Error, c.DiffRemoveBg, 4.5},
		{"markdown heading on background", theme.Markdown.Heading, c.Background, 4.5},
		{"markdown link on background", theme.Markdown.Link, c.Background, 4.5},
	}

	for _, check := range checks {
		ratio, ok := contrastRatioHex(check.fg, check.bg)
		require.True(t, ok, "%s: invalid colors %q on %q", check.name, check.fg, check.bg)
		assert.GreaterOrEqual(t, ratio, check.minRatio,
			"%s: contrast of %s on %s is %.2f, want >= %.2f", check.name, check.fg, check.bg, ratio, check.minRatio)
	}
}

// TestEmphasisStylesReadableOnLightTheme guards against emphasized text drawn
// directly on the app background (key hints such as "Esc to interrupt",
// palette categories, the active resize handle) using the selection
// foreground. That color is near-white in light themes, which made the text
// invisible; these styles must stay readable after applying the light theme.
func TestEmphasisStylesReadableOnLightTheme(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide style variables.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	theme, err := LoadTheme(DefaultLightThemeRef)
	require.NoError(t, err)
	ApplyTheme(theme)

	checks := []struct {
		name  string
		style lipgloss.Style
		bg    color.Color
	}{
		{"HighlightWhiteStyle", HighlightWhiteStyle, Background},
		{"PaletteCategoryStyle", PaletteCategoryStyle, Background},
		{"ResizeHandleActiveStyle", ResizeHandleActiveStyle, Background},
		{"ThumbActiveStyle", ThumbActiveStyle, BackgroundAlt},
	}
	for _, check := range checks {
		ratio := contrastRatio(check.style.GetForeground(), check.bg)
		assert.GreaterOrEqual(t, ratio, 4.5,
			"%s foreground must be readable on the light theme background", check.name)
	}
}
