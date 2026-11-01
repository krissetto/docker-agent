package styles

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewRestFgAbsentKeepsLegacyFade(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme, err := loadBuiltinTheme(LegacyDefaultThemeRef)
	require.NoError(t, err)
	require.Empty(t, theme.Colors.ReviewRestFg)
	ApplyTheme(theme)
	assert.Nil(t, ReviewRestFg(), "absent review_rest_fg keeps the whole-row fade")
}

func TestReviewRestFgPresentInDockerNext(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	for _, tt := range []struct{ ref, want string }{
		{DefaultThemeRef, "#97918a"},
		{DockerNextLightThemeRef, "#64605c"},
	} {
		theme, err := loadBuiltinTheme(tt.ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		require.NotNil(t, ReviewRestFg(), tt.ref)
		assert.Equal(t, tt.want, RGBToHex(ColorToRGB(ReviewRestFg())), tt.ref)
	}
}

func TestDiffAddEmphBgOverrideAndDerivedFallback(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	legacy, err := loadBuiltinTheme(LegacyDefaultThemeRef)
	require.NoError(t, err)
	ApplyTheme(legacy)
	derived := RGBToHex(ColorToRGB(DiffAddEmphBg))

	dark, err := loadBuiltinTheme(DefaultThemeRef)
	require.NoError(t, err)
	require.Equal(t, "#0D3823", dark.Colors.DiffAddEmphBg)
	ApplyTheme(dark)
	assert.Equal(t, "#0d3823", RGBToHex(ColorToRGB(DiffAddEmphBg)), "explicit diff_add_emph_bg wins")

	legacy.Colors.DiffAddEmphBg = "not-a-color"
	ApplyTheme(legacy)
	assert.Equal(t, derived, RGBToHex(ColorToRGB(DiffAddEmphBg)), "unparseable value keeps the derived fill")
}

func TestMergeColorsReviewAndDiffPresence(t *testing.T) {
	t.Parallel()
	base := ThemeColors{ReviewRestFg: "#111111", DiffAddEmphBg: "#222222"}
	merged := mergeColors(base, ThemeColors{})
	assert.Equal(t, "#111111", merged.ReviewRestFg, "absent override keeps base")
	assert.Equal(t, "#222222", merged.DiffAddEmphBg, "absent override keeps base")
	merged = mergeColors(base, ThemeColors{ReviewRestFg: "#333333", DiffAddEmphBg: "#444444"})
	assert.Equal(t, "#333333", merged.ReviewRestFg)
	assert.Equal(t, "#444444", merged.DiffAddEmphBg)
}

func TestDiffAddEmphBgAbsentIsDerivedExactly(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	legacy, err := loadBuiltinTheme(LegacyDefaultThemeRef)
	require.NoError(t, err)
	require.Empty(t, legacy.Colors.DiffAddEmphBg)
	ApplyTheme(legacy)
	want := RGBToHex(ColorToRGB(deriveDiffEmphasisBg(legacy.Colors.DiffAddBg, legacy.Colors.Success)))
	assert.Equal(t, want, RGBToHex(ColorToRGB(DiffAddEmphBg)), "absent diff_add_emph_bg is the derived fill")
}
