package markdown

import (
	"image/color"
	"math"
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func renderedStyleForText(rows []uv.Line, text string) (uv.Style, bool) {
	if text == "" {
		return uv.Style{}, false
	}
	for _, row := range rows {
		for start, cell := range row {
			if cell.Content != text[:1] || start+len(text) > len(row) {
				continue
			}
			matched := true
			for offset, r := range text {
				if row[start+offset].Content != string(r) {
					matched = false
					break
				}
			}
			if matched {
				return cell.Style, true
			}
		}
	}
	return uv.Style{}, false
}

func renderedContrast(foreground, background color.Color) float64 {
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

func TestFastMarkdownRenderedTextContrastAcrossBuiltinThemes(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
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

			const markdown = "# HEADING_MARKER\n\n[LINK_MARKER](https://example.com) and `INLINE_CODE_MARKER`\n\n```go\n// COMMENT_MARKER\nfunc marker() string { return \"STRING_MARKER\" }\nvar count = 42\n```\n\n```python\nclass CLASS_MARKER:\n    pass\nbroken = $\n```"
			rendered, err := NewFastRenderer(120).HideCopyIcon().Render(markdown)
			require.NoError(t, err)
			rows := uv.NewStyledString(rendered).Lines(ansi.GraphemeWidth)

			codeBg := theme.Markdown.CodeBg
			if codeBg == "" {
				codeBg = theme.Colors.BackgroundAlt
			}
			errorBg := theme.Chroma.ErrorBg
			require.NotEmpty(t, errorBg, "theme includes its effective Chroma error background")
			markers := []struct {
				text       string
				background color.Color
				styledBg   bool
			}{
				{"HEADING_MARKER", lipgloss.Color(theme.Colors.Background), false},
				{"LINK_MARKER", lipgloss.Color(theme.Colors.Background), false},
				{"INLINE_CODE_MARKER", lipgloss.Color(codeBg), true},
				{"COMMENT_MARKER", lipgloss.Color(codeBg), true},
				{"func", lipgloss.Color(codeBg), true},
				{"marker", lipgloss.Color(codeBg), true},
				{"STRING_MARKER", lipgloss.Color(codeBg), true},
				{"42", lipgloss.Color(codeBg), true},
				{"CLASS_MARKER", lipgloss.Color(codeBg), true},
				{"$", lipgloss.Color(errorBg), true},
			}
			for _, marker := range markers {
				style, ok := renderedStyleForText(rows, marker.text)
				require.True(t, ok, "rendered markdown contains %q: %q", marker.text, ansi.Strip(rendered))
				require.NotNil(t, style.Fg, "%s has an actual ANSI foreground", marker.text)
				background := style.Bg
				if marker.styledBg {
					require.NotNil(t, background, "%s carries its semantic code/token surface in the rendered ANSI", marker.text)
					require.Equal(t, color.NRGBAModel.Convert(marker.background), color.NRGBAModel.Convert(background), "%s uses its rendered semantic code/token surface", marker.text)
				} else if background == nil {
					background = lipgloss.Color(theme.Colors.Background)
				}
				ratio := renderedContrast(style.Fg, background)
				require.GreaterOrEqualf(t, ratio, 4.5, "%s rendered contrast is %.2f:1 on theme %s", marker.text, ratio, ref)
			}
		})
	}
}
