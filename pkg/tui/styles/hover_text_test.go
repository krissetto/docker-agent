package styles

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Literal expectations are computed independently (sRGB lerp, round half up):
// light primary #101010 -> #3345D6 at 0.5 is #222b73; dark primary #DDD4C9 -> #919CFD at 0.5 is #b7b8e3;
// dark bright #FFFDFA -> #919CFD at 0.5 is #c8cdfc and at 1 is #919cfd.

func hoverTextFG(t *testing.T, text string) string {
	t.Helper()
	cells := hoverCells(text)
	require.Len(t, cells, 1)
	require.NotNil(t, cells[0].fg)
	return RGBToHex(ColorToRGB(cells[0].fg))
}

func TestHoverTextAbsentKeepsLegacyLift(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme, err := loadBuiltinTheme(LegacyDefaultThemeRef)
	require.NoError(t, err)
	require.Empty(t, theme.Colors.HoverText)
	ApplyTheme(theme)
	require.Nil(t, hoverTextColor)
	base := lipgloss.NewStyle().Foreground(TextPrimary).Render("X")
	assert.Equal(t, RGBToHex(ColorToRGB(Brighten(TextPrimary, .25))), hoverTextFG(t, HoverText(base, 1, TextPrimary)))
	assert.Equal(t, RGBToHex(ColorToRGB(Brighten(TextPrimary, .125))), hoverTextFG(t, HoverText(base, .5, TextPrimary)))
}

func TestHoverTextPresentLightPrimaryLerpsExactly(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme, err := loadBuiltinTheme(DockerNextLightThemeRef)
	require.NoError(t, err)
	ApplyTheme(theme)
	require.NotNil(t, hoverTextColor)
	base := lipgloss.NewStyle().Foreground(TextPrimary).Render("X")
	assert.Equal(t, "#3345d6", hoverTextFG(t, HoverText(base, 1, TextPrimary)), "full hover is the authored target")
	assert.Equal(t, "#222b73", hoverTextFG(t, HoverText(base, .5, TextPrimary)), "half hover is the sRGB midpoint")
	assert.Equal(t, base, HoverText(base, 0, TextPrimary), "zero progress is byte-identical")
}

func TestHoverTextPresentDarkPrimaryAndBrightLerp(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme, err := loadBuiltinTheme(DefaultThemeRef)
	require.NoError(t, err)
	ApplyTheme(theme)
	primary := lipgloss.NewStyle().Foreground(TextPrimary).Render("X")
	bright := lipgloss.NewStyle().Foreground(TextBright).Render("X")
	assert.Equal(t, "#b7b8e3", hoverTextFG(t, HoverText(primary, .5, TextPrimary)))
	assert.Equal(t, "#c8cdfc", hoverTextFG(t, HoverText(bright, .5, TextPrimary)))
	assert.Equal(t, "#919cfd", hoverTextFG(t, HoverText(bright, 1, TextPrimary)))
}

func TestHoverTextFallbackNilUsesPrimaryRole(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme, err := loadBuiltinTheme(DockerNextLightThemeRef)
	require.NoError(t, err)
	ApplyTheme(theme)
	assert.Equal(t, "#3345d6", hoverTextFG(t, HoverText("X", 1, TextPrimary)), "uncolored text with a primary fallback uses hover_text")
	assert.Equal(t, RGBToHex(ColorToRGB(shiftLightness(Warning, hoverLightness))), hoverTextFG(t, HoverText("X", 1, Warning)), "non-primary fallback keeps lightness")
}

func TestHoverTextStatusKeepsLightnessWhenPresent(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme, err := loadBuiltinTheme(DockerNextLightThemeRef)
	require.NoError(t, err)
	ApplyTheme(theme)
	ok := lipgloss.NewStyle().Foreground(Success).Render("o")
	got := hoverTextFG(t, HoverText(ok, 1, TextPrimary))
	assert.Equal(t, RGBToHex(ColorToRGB(shiftLightness(Success, hoverLightness))), got)
	assert.NotEqual(t, "#3345d6", got)
}

func TestResolveHoverTextParsing(t *testing.T) {
	t.Parallel()
	assert.Nil(t, resolveHoverText(""), "absent")
	assert.Nil(t, resolveHoverText("#zzzzzz"), "invalid hex is treated as absent")
	assert.Equal(t, "#aabbcc", RGBToHex(ColorToRGB(resolveHoverText("#abc"))), "short form")
	assert.Equal(t, "#3345d6", RGBToHex(ColorToRGB(resolveHoverText("#3345D6"))), "long form")
}

func TestMergeColorsHoverTextPresence(t *testing.T) {
	t.Parallel()
	base := ThemeColors{HoverText: "#111111"}
	assert.Equal(t, "#111111", mergeColors(base, ThemeColors{}).HoverText, "absent override keeps base")
	assert.Equal(t, "#222222", mergeColors(base, ThemeColors{HoverText: "#222222"}).HoverText, "override wins")
}
