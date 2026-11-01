package styles

import (
	"fmt"
	"image/color"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// defaultHoverLightness is the HSL lightness lift applied at full hover when a
// theme omits hover_lightness; it is the legacy behavior every existing theme keeps.
const defaultHoverLightness = 0.25

// hoverLightness is the resolved delta for the current theme, set by ApplyTheme.
var hoverLightness = defaultHoverLightness

// resolveHoverLightness maps a theme's optional hover_lightness to the effective
// delta. Absent keeps defaultHoverLightness. Explicit values are clamped to
// [-1, 1] so HSL lightness stays in range; NaN falls back to the default.
func resolveHoverLightness(v *float64) float64 {
	if v == nil || math.IsNaN(*v) {
		return defaultHoverLightness
	}
	return min(1, max(-1, *v))
}

// reviewRestFg is the optional muted filename color for resting file rows in the
// review section, set by ApplyTheme. nil keeps the legacy whole-row fade.
var reviewRestFg color.Color

// ReviewRestFg returns the resting filename color for review rows, or nil when the theme
// keeps the legacy whole-row fade.
func ReviewRestFg() color.Color { return reviewRestFg }

// resolveColorHex parses an optional hex color. Empty or unparseable values resolve to nil.
func resolveColorHex(v string) color.Color {
	if v == "" {
		return nil
	}
	r, g, b, ok := parseHexRGB(v)
	if !ok {
		return nil
	}
	return RGBToColor(r, g, b)
}

// hoverTextColor is the optional explicit hover target for primary and bright
// text, set by ApplyTheme. nil keeps the lightness-only hover for every theme
// that does not author hover_text.
var hoverTextColor color.Color

// resolveHoverText maps a theme's optional hover_text to a color. Absent or
// unparseable values resolve to nil, which keeps the legacy hover path.
func resolveHoverText(v string) color.Color {
	return resolveColorHex(v)
}

// isHoverTextRole reports whether c is exactly the primary or bright text color.
// Only those cells move toward hover_text; other foregrounds keep the lightness shift.
func isHoverTextRole(c color.Color) bool {
	r, g, b := rgb8(c)
	for _, role := range []color.Color{TextPrimary, TextBright} {
		rr, gg, bb := rgb8(role)
		if r == rr && g == gg && b == bb {
			return true
		}
	}
	return false
}

func rgb8(c color.Color) (r, g, b int) {
	ri, gi, bi, _ := c.RGBA()
	return int(ri >> 8), int(gi >> 8), int(bi >> 8)
}

// lerpHoverText moves from toward to in sRGB by progress in [0, 1].
func lerpHoverText(from, to color.Color, progress float64) color.Color {
	fr, fg, fb := rgb8(from)
	tr, tg, tb := rgb8(to)
	mix := func(a, b int) float64 { return (float64(a) + (float64(b)-float64(a))*progress) / 255 }
	return RGBToColor(mix(fr, tr), mix(fg, tg), mix(fb, tb))
}

// hoverColor picks the hover foreground for one cell: primary and bright text
// lerp toward hover_text when the theme authors it, everything else shifts HSL lightness.
func hoverColor(original color.Color, progress float64) color.Color {
	if hoverTextColor != nil && isHoverTextRole(original) {
		return lerpHoverText(original, hoverTextColor, progress)
	}
	return shiftLightness(original, hoverLightness*progress)
}

// shiftLightness moves c's HSL lightness by delta, clamped to [0, 1]. Non-negative
// deltas go through Brighten, so the legacy lift is byte-identical.
func shiftLightness(c color.Color, delta float64) color.Color {
	switch {
	case delta == 0:
		return c
	case delta > 0:
		return Brighten(c, delta)
	}
	r, g, b := ColorToRGB(c)
	h, s, l := rgbToHSL(r, g, b)
	r, g, b = hslToRGB(h, s, max(0, l+delta))
	return RGBToColor(r, g, b)
}

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
			out.WriteString(hoverForegroundSequence(hoverColor(original, progress)))
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
