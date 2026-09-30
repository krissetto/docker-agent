package styles

import (
	uv "github.com/charmbracelet/ultraviolet"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestPaneDimExtendedColorsAndResets(t *testing.T) {
	fc := NewFadeContextRGB(0, 0, 0, 200, 200, 200)
	for _, prefix := range []string{"38", "48", "58"} {
		for _, color := range []string{";2;100;150;200", ":2:100:150:200", ":2::100:150:200", ":2:0:100:150:200", ";5;196", ":5:196"} {
			input := "\x1b[" + prefix + color + "m界λ\x1b[0m plain"
			output := FadeLineCtx(input, 0.62, &fc)
			require.Equal(t, "界λ plain", ansi.Strip(output))
			require.Contains(t, output, prefix)
		}
	}
	input := "\x1b[48;2;100;0;0mred\x1b[49mnormal\x1b[58;2;0;100;0mline\x1b[59mdefault"
	output := FadeLineCtx(input, 0.62, &fc)
	require.Contains(t, output, "\x1b[49m")
	require.Contains(t, output, "\x1b[59m")
	require.Equal(t, ansi.Strip(input), ansi.Strip(output))
}

func TestPaneDimMalformedColorsDoNotBecomeResets(t *testing.T) {
	fc := NewFadeContextRGB(0, 0, 0, 200, 200, 200)
	for _, input := range []string{"\x1b[38;2;0mtext", "\x1b[48:2:0;1mtext", "\x1b[58:5mtext"} {
		output := FadeLineCtx(input, 0.62, &fc)
		require.Equal(t, "text", ansi.Strip(output))
		require.NotContains(t, output, "\x1b[0m", "channel zero is not an SGR reset")
	}
}

func TestFadeAndPaneDimPreserveDefaultBackgroundCells(t *testing.T) {
	fc := NewFadeContextRGB(28, 28, 34, 224, 224, 227)
	input := "default \x1b[48;2;90;100;110mselected\x1b[49m tail"
	for _, opacity := range []float64{0.1, 0.5, 0.62, 1} {
		output := FadeLineCtx(input, opacity, &fc)
		cells := uv.NewStyledString(output).Lines(ansi.GraphemeWidth)[0]
		for i, cell := range cells {
			if i >= len("default ") && i < len("default selected") {
				require.NotNil(t, cell.Style.Bg, "explicit selection background survives fade/dimming")
			} else {
				require.Nil(t, cell.Style.Bg, "default background never becomes opaque")
			}
		}
	}
}
