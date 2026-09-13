package styles

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hoverCell struct {
	glyph  string
	fg, bg color.Color
}

func hoverCells(line string) []hoverCell {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var fg, bg color.Color
	var state byte
	var cells []hoverCell
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, parser)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				fg, bg = nil, nil
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); {
				case param == 0:
					fg, bg = nil, nil
				case param == 39:
					fg = nil
				case param == 49:
					bg = nil
				case param == 38 || param == 48 || param == 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							fg = c
						}
						if param == 48 {
							bg = c
						}
						i += consumed - 1
					}
				case param >= 30 && param <= 37:
					fg = ansi.Black + ansi.BasicColor(param-30)
				case param >= 90 && param <= 97:
					fg = ansi.BrightBlack + ansi.BasicColor(param-90)
				}
			}
		}
		for range width {
			cells = append(cells, hoverCell{seq, fg, bg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func TestHoverTextPreservesMulticolorANSIAndGeometry(t *testing.T) {
	t.Parallel()
	fallback := lipgloss.Color("#345678")
	text := ansi.SetHyperlink("https://example.com") + "\x1b[1;3;4;48;2;12;34;56mA" +
		"\x1b[38;5;39m界" + "\x1b[38;2;90;120;150mB" + "\x1b[0mC" + "\x1b[31mD" +
		"\x1b[38:2::20:40:60mE\x1b[38:5:120mF\x1b[39mG" + ansi.ResetHyperlink()
	require.Equal(t, text, HoverText(text, 0, fallback))
	before := hoverCells(text)
	for _, progress := range []float64{0.25, 0.5, 1} {
		got := HoverText(text, progress, fallback)
		assert.Equal(t, ansi.Strip(text), ansi.Strip(got))
		assert.Equal(t, lipgloss.Width(text), lipgloss.Width(got))
		assert.Contains(t, got, "\x1b[1;3;4;48;2;12;34;56m")
		assert.Equal(t, strings.Count(text, ansi.SetHyperlink("https://example.com")), strings.Count(got, ansi.SetHyperlink("https://example.com")))
		assert.Equal(t, strings.Count(text, ansi.ResetHyperlink()), strings.Count(got, ansi.ResetHyperlink()))
		after := hoverCells(got)
		require.Len(t, after, len(before))
		for i, cell := range before {
			original := cell.fg
			if original == nil {
				original = fallback
			}
			require.NotNil(t, after[i].fg)
			assert.Equal(t, RGBToHex(ColorToRGB(Brighten(original, .25*progress))), RGBToHex(ColorToRGB(after[i].fg)))
			assert.Equal(t, cell.bg, after[i].bg)
		}
	}
	assert.Equal(t, "plain", HoverText("plain", 1, nil))
	assert.Equal(t, HoverText(text, 1, fallback), HoverText(text, 2, fallback))
}

func TestHoverTextWarmThemesRebaseOriginalColorsAtCurrentProgress(t *testing.T) {
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		for _, fg := range []color.Color{TextPrimary, TextMuted, Warning, Error, AgentIdentityStyle("unknown", false).GetForeground()} {
			base := lipgloss.NewStyle().Foreground(fg).Background(EditorBg).Render("X")
			got := HoverText(base, .5, TextPrimary)
			cells := hoverCells(got)
			require.Len(t, cells, 1)
			require.NotNil(t, cells[0].fg)
			require.NotNil(t, cells[0].bg)
			assert.Equal(t, RGBToHex(ColorToRGB(Brighten(fg, .125))), RGBToHex(ColorToRGB(cells[0].fg)))
			assert.Equal(t, RGBToHex(ColorToRGB(EditorBg)), RGBToHex(ColorToRGB(cells[0].bg)))
			assert.Equal(t, base, HoverText(base, 0, TextPrimary))
			assert.Equal(t, got, HoverText(base, .5, TextPrimary), "settled rendering is deterministic")
		}
	}
}
