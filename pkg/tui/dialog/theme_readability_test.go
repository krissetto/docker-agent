package dialog

import (
	"image/color"
	"math"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSelectedPickerBadgeContrastAcrossBuiltinThemes(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		if !styles.IsBuiltinTheme(ref) {
			continue
		}
		t.Run(ref, func(t *testing.T) {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)

			checkSelected := func(label, rendered string, marker string) {
				t.Helper()
				style := requireRenderedTextStyle(t, rendered, marker)
				checkBadgeCellContrast(t, style, ref, label)
			}
			checkUnselected := func(label, rendered, marker string, want color.Color) {
				t.Helper()
				style := requireRenderedTextStyle(t, rendered, marker)
				require.NotNil(t, style.Fg, "%s unselected badge has ANSI foreground", label)
				require.Equal(t, color.NRGBAModel.Convert(want), color.NRGBAModel.Convert(style.Fg), "%s keeps its semantic unselected foreground", label)
			}

			for _, model := range []runtime.ModelChoice{
				{Name: "model", IsCurrent: true},
				{Name: "model", IsDefault: true},
				{Name: "model", IsCurrent: true, Provider: "", Model: "one/two,three/four"},
			} {
				selected := pickerRowStyles(true)
				selectedModel := renderRowName(model, 60, selected)
				if model.IsCurrent {
					checkSelected("model current", selectedModel, "(current)")
				}
				if model.IsDefault {
					checkSelected("model default", selectedModel, "(default)")
				}
				if isAlloyModel(model) {
					checkSelected("model alloy", selectedModel, "(alloy)")
				}

				unselectedModel := renderRowName(model, 60, pickerRowStyles(false))
				if model.IsCurrent {
					checkUnselected("model current", unselectedModel, "(current)", styles.BadgeGreen)
				}
				if model.IsDefault {
					checkUnselected("model default", unselectedModel, "(default)", styles.BadgeCyan)
				}
				if isAlloyModel(model) {
					checkUnselected("model alloy", unselectedModel, "(alloy)", styles.BadgePurple)
				}
			}

			for _, choice := range []ThemeChoice{
				{Name: "theme", IsCurrent: true},
				{Name: "theme", IsDefault: true},
			} {
				selected := (&themePickerDialog{}).renderTheme(choice, true, 80)
				unselected := (&themePickerDialog{}).renderTheme(choice, false, 80)
				if choice.IsCurrent {
					checkSelected("theme current", selected, "(current)")
					checkUnselected("theme current", unselected, "(current)", styles.BadgeGreen)
				} else {
					checkSelected("theme default", selected, "(default)")
					checkUnselected("theme default", unselected, "(default)", styles.BadgeCyan)
				}
			}

			effortDialog := &effortPickerDialog{current: effort.High}
			checkSelected("effort current", effortDialog.renderLevel(effort.High, true), "(current)")
			checkUnselected("effort current", effortDialog.renderLevel(effort.High, false), "(current)", styles.BadgeGreen)
		})
	}
}

func requireRenderedTextStyle(t *testing.T, rendered, marker string) uv.Style {
	t.Helper()
	rows := uv.NewStyledString(rendered).Lines(ansi.GraphemeWidth)
	for _, row := range rows {
		for start, cell := range row {
			if cell.Content != marker[:1] || start+len(marker) > len(row) {
				continue
			}
			matched := true
			for offset, r := range marker {
				if row[start+offset].Content != string(r) {
					matched = false
					break
				}
			}
			if matched {
				return cell.Style
			}
		}
	}
	require.FailNow(t, "rendered text not found", "%q in %q", marker, ansi.Strip(rendered))
	return uv.Style{}
}

func checkBadgeCellContrast(t *testing.T, style uv.Style, theme, badge string) {
	t.Helper()
	require.NotNil(t, style.Fg, "%s %s badge has actual ANSI foreground", theme, badge)
	require.NotNil(t, style.Bg, "%s %s badge has actual ANSI background", theme, badge)
	require.Equal(t, color.NRGBAModel.Convert(styles.White), color.NRGBAModel.Convert(style.Fg), "%s %s selected badge uses selected row foreground", theme, badge)
	require.Equal(t, color.NRGBAModel.Convert(styles.MobyBlue), color.NRGBAModel.Convert(style.Bg), "%s %s selected badge uses selected row background", theme, badge)
	ratio := renderedContrastRatio(style.Fg, style.Bg)
	require.GreaterOrEqualf(t, ratio, 4.5, "%s %s selected badge ANSI cell contrast %.2f:1", theme, badge, ratio)
}

func renderedContrastRatio(foreground, background color.Color) float64 {
	luminance := func(c color.Color) float64 {
		r, g, b, _ := color.NRGBAModel.Convert(c).(color.NRGBA).RGBA()
		toLinear := func(v uint32) float64 {
			srgb := float64(v) / 65535
			if srgb <= 0.04045 {
				return srgb / 12.92
			}
			return math.Pow((srgb+0.055)/1.055, 2.4)
		}
		return 0.2126*toLinear(r) + 0.7152*toLinear(g) + 0.0722*toLinear(b)
	}
	fg, bg := luminance(foreground), luminance(background)
	return (max(fg, bg) + 0.05) / (min(fg, bg) + 0.05)
}
