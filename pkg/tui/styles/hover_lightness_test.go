package styles

import (
	"math"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveHoverLightness(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   *float64
		want float64
	}{
		{"absent keeps legacy lift", nil, 0.25},
		{"explicit zero means no shift", new(0.0), 0.0},
		{"positive passes through", new(0.1), 0.1},
		{"negative darkens", new(-0.2), -0.2},
		{"clamps high to one", new(7.0), 1},
		{"clamps low to minus one", new(-3.0), -1},
		{"infinity clamps", new(math.Inf(1)), 1},
		{"negative infinity clamps", new(math.Inf(-1)), -1},
		{"NaN falls back to legacy lift", new(math.NaN()), 0.25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.InDelta(t, tt.want, resolveHoverLightness(tt.in), 1e-12)
		})
	}
}

func TestHoverLightnessYAMLPresence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
		want *float64
	}{
		{"absent", "text_primary: \"#000000\"\n", nil},
		{"explicit zero is present", "hover_lightness: 0\n", new(0.0)},
		{"positive", "hover_lightness: 0.1\n", new(0.1)},
		{"negative", "hover_lightness: -0.20\n", new(-0.2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var colors ThemeColors
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &colors))
			if tt.want == nil {
				assert.Nil(t, colors.HoverLightness)
				return
			}
			require.NotNil(t, colors.HoverLightness)
			assert.InDelta(t, *tt.want, *colors.HoverLightness, 1e-12)
		})
	}
}

func TestMergeColorsHoverLightnessPresence(t *testing.T) {
	t.Parallel()
	base := ThemeColors{HoverLightness: new(-0.2)}
	assert.Equal(t, base.HoverLightness, mergeColors(base, ThemeColors{}).HoverLightness, "absent override keeps base")
	merged := mergeColors(base, ThemeColors{HoverLightness: new(0.0)})
	require.NotNil(t, merged.HoverLightness, "explicit zero override survives merge")
	assert.InDelta(t, 0, *merged.HoverLightness, 1e-12)
	assert.Nil(t, mergeColors(ThemeColors{}, ThemeColors{}).HoverLightness, "absent everywhere stays absent")
}

func TestShiftLightnessSemantics(t *testing.T) {
	t.Parallel()
	c := lipgloss.Color("#3A6EA5")
	assert.Equal(t, RGBToHex(ColorToRGB(c)), RGBToHex(ColorToRGB(shiftLightness(c, 0))), "zero delta is unchanged")
	assert.Equal(t, RGBToHex(ColorToRGB(Brighten(c, 0.25))), RGBToHex(ColorToRGB(shiftLightness(c, 0.25))), "positive delta is the legacy Brighten path")
	assert.Equal(t, "#89aed6", RGBToHex(ColorToRGB(shiftLightness(c, 0.25))), "literal +0.25 legacy lift, independent of Brighten")
	assert.Equal(t, "#1f3c5a", RGBToHex(ColorToRGB(shiftLightness(c, -0.2))), "literal -0.20 darkening, independent of the HSL helper")

	darker := shiftLightness(c, -0.2)
	r, g, b := ColorToRGB(c)
	_, _, l0 := rgbToHSL(r, g, b)
	r, g, b = ColorToRGB(darker)
	_, _, l1 := rgbToHSL(r, g, b)
	assert.InDelta(t, math.Max(0, l0-0.2), l1, 0.01, "negative delta lowers HSL lightness")

	assert.Equal(t, "#000000", RGBToHex(ColorToRGB(shiftLightness(lipgloss.Color("#101010"), -1))), "lightness clamps at black")
	white := lipgloss.Color("#FFFFFF")
	r, g, b = ColorToRGB(shiftLightness(white, -0.2))
	_, _, lw := rgbToHSL(r, g, b)
	assert.InDelta(t, 0.8, lw, 0.01)
}

func TestApplyThemeHoverLightnessFollowsThemeKey(t *testing.T) {
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	for _, tt := range []struct {
		name  string
		value *float64
		want  float64
	}{
		{"absent", nil, 0.25},
		{"zero", new(0.0), 0},
		{"negative", new(-0.2), -0.2},
		{"out of range", new(5.0), 1},
	} {
		theme, err := loadBuiltinTheme(DefaultThemeRef)
		require.NoError(t, err)
		theme.Colors.HoverLightness = tt.value
		ApplyTheme(theme)
		assert.InDelta(t, tt.want, hoverLightness, 1e-12, tt.name)
	}
}

func TestDockerNextHoverLightnessPolicy(t *testing.T) {
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	dark, err := loadBuiltinTheme("docker-next-dark")
	require.NoError(t, err)
	assert.Nil(t, dark.Colors.HoverLightness, "dark keeps the legacy absent key")
	ApplyTheme(dark)
	assert.InDelta(t, 0.25, hoverLightness, 1e-12)

	light, err := loadBuiltinTheme("docker-next-light")
	require.NoError(t, err)
	require.NotNil(t, light.Colors.HoverLightness)
	ApplyTheme(light)
	assert.InDelta(t, -0.20, hoverLightness, 1e-12)

	// Primary text moves to the authored hover_text target (lerp), not a lightness shift.
	base := lipgloss.NewStyle().Foreground(TextPrimary).Background(EditorBg).Render("X")
	got := hoverCells(HoverText(base, 1, TextPrimary))
	require.Len(t, got, 1)
	assert.Equal(t, "#3345d6", RGBToHex(ColorToRGB(got[0].fg)), "light primary hover is the hover_text target")
	// Other foregrounds keep the authored darkening.
	success := lipgloss.NewStyle().Foreground(Success).Background(EditorBg).Render("o")
	gotSuccess := hoverCells(HoverText(success, 1, TextPrimary))
	require.Len(t, gotSuccess, 1)
	beforeSuccess := hoverCells(success)
	require.Len(t, beforeSuccess, 1)
	_, _, lBefore := rgbToHSL(ColorToRGB(beforeSuccess[0].fg))
	_, _, lAfter := rgbToHSL(ColorToRGB(gotSuccess[0].fg))
	assert.Less(t, lAfter, lBefore, "light status hover keeps the darkening")
}

func TestBuiltinThemesHoverLightnessOnlyChangesDockerNextLight(t *testing.T) {
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		want := 0.25
		if ref == "docker-next-light" {
			want = -0.20
		}
		assert.InDelta(t, want, hoverLightness, 1e-12, ref)
	}
}
