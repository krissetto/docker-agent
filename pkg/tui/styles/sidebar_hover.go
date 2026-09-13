package styles

import (
	"fmt"
	"image/color"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// HoverText highlights each original foreground without changing styles or geometry.
// Callers supply fresh text and own the animation progress and uncolored-text fallback.
func HoverText(text string, progress float64, fallback color.Color) string {
	if progress <= 0 || math.IsNaN(progress) || text == "" {
		return text
	}
	progress = min(progress, 1)
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var foreground color.Color
	var state byte
	var out strings.Builder
	for text != "" {
		seq, width, n, next := ansi.DecodeSequence(text, state, parser)
		if n == 0 {
			out.WriteString(text)
			break
		}
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			foreground = hoverForeground(parser.Params(), foreground)
		}
		original := foreground
		if original == nil {
			original = fallback
		}
		if width > 0 && original != nil {
			out.WriteString(hoverForegroundSequence(Brighten(original, 0.25*progress)))
			out.WriteString(seq)
			if foreground == nil {
				out.WriteString("\x1b[39m")
			} else {
				out.WriteString(hoverForegroundSequence(foreground))
			}
		} else {
			out.WriteString(seq)
		}
		state, text = next, text[n:]
	}
	return out.String()
}

func hoverForegroundSequence(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r>>8, g>>8, b>>8)
}

func hoverForeground(params ansi.Params, foreground color.Color) color.Color {
	if len(params) == 0 {
		return nil
	}
	for i := 0; i < len(params); i++ {
		switch param := params[i].Param(0); {
		case param == 0 || param == 39:
			foreground = nil
		case param == 38 || param == 48 || param == 58:
			var c color.Color
			if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
				if param == 38 {
					foreground = c
				}
				i += consumed - 1
			}
		case param >= 30 && param <= 37:
			foreground = ansi.Black + ansi.BasicColor(param-30)
		case param >= 90 && param <= 97:
			foreground = ansi.BrightBlack + ansi.BasicColor(param-90)
		}
	}
	return foreground
}
