package styles

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// FadeContext holds pre-computed background and default-foreground RGB values
// used by FadeLine to interpolate colors toward the theme background.
type FadeContext struct {
	bgR, bgG, bgB          float64
	defFgR, defFgG, defFgB float64
}

// NewFadeContext returns a FadeContext derived from the current theme's
// background and text-primary colors.
func NewFadeContext() FadeContext {
	theme := CurrentTheme()
	bgR, bgG, bgB := fadeParseHexRGB(theme.Colors.Background)
	fgR, fgG, fgB := fadeParseHexRGB(theme.Colors.TextPrimary)
	return FadeContext{bgR: bgR, bgG: bgG, bgB: bgB, defFgR: fgR, defFgG: fgG, defFgB: fgB}
}

// NewFadeContextRGB returns a FadeContext with explicit background and
// default-foreground RGB values. Useful for testing with a known palette.
func NewFadeContextRGB(bgR, bgG, bgB, fgR, fgG, fgB float64) FadeContext {
	return FadeContext{bgR: bgR, bgG: bgG, bgB: bgB, defFgR: fgR, defFgG: fgG, defFgB: fgB}
}

func (fc *FadeContext) interpolate(r, g, b, alpha float64) (int, int, int) {
	return fadeClamp(fc.bgR + alpha*(r-fc.bgR)),
		fadeClamp(fc.bgG + alpha*(g-fc.bgG)),
		fadeClamp(fc.bgB + alpha*(b-fc.bgB))
}

func fadeClamp(v float64) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return int(v)
}

// FadeLine interpolates every foreground and background color in a
// pre-rendered ANSI string toward the current theme background by alpha
// (0.0 = invisible, 1.0 = unchanged). Non-color attributes (bold, italic,
// etc.) are preserved. A new FadeContext is created from the current theme
// on each call; use FadeLineCtx for hot loops where the context can be
// reused.
func FadeLine(line string, alpha float64) string {
	fc := NewFadeContext()
	return FadeLineCtx(line, alpha, &fc)
}

// FadeLineCtx is the same as FadeLine but accepts a pre-built FadeContext,
// avoiding repeated theme lookups when fading many lines in a batch.
func FadeLineCtx(line string, alpha float64, fc *FadeContext) string {
	if !strings.Contains(line, "\x1b") {
		if strings.TrimSpace(line) == "" {
			return line
		}
		r, g, b := fc.interpolate(fc.defFgR, fc.defFgG, fc.defFgB, alpha)
		var buf strings.Builder
		buf.Grow(len(line) + 32)
		buf.WriteString("\x1b[38;2;")
		buf.WriteString(strconv.Itoa(r))
		buf.WriteByte(';')
		buf.WriteString(strconv.Itoa(g))
		buf.WriteByte(';')
		buf.WriteString(strconv.Itoa(b))
		buf.WriteByte('m')
		buf.WriteString(line)
		buf.WriteString("\x1b[m")
		return buf.String()
	}

	p := ansi.GetParser()
	defer ansi.PutParser(p)

	var buf strings.Builder
	buf.Grow(len(line) + 64)

	input := []byte(line)
	var state byte
	foregroundSet := false

	for len(input) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(input, state, p)
		state = newState

		if width > 0 {
			if !foregroundSet {
				writeFadedDefaultForeground(&buf, alpha, fc)
				foregroundSet = true
			}
			buf.Write(seq)
			input = input[n:]
			continue
		}

		cmd := ansi.Cmd(p.Command())
		if cmd.Final() == 'm' && cmd.Intermediate() == 0 && cmd.Prefix() == 0 {
			params := p.Params()
			fadeRewriteSGR(&buf, params, alpha, fc)
			foregroundSet = foregroundSet || fadeSGRSetsForeground(params)
		} else {
			buf.Write(seq)
		}
		input = input[n:]
	}
	buf.WriteString("\x1b[m")
	return buf.String()
}

func writeFadedDefaultForeground(buf *strings.Builder, alpha float64, fc *FadeContext) {
	r, g, b := fc.interpolate(fc.defFgR, fc.defFgG, fc.defFgB, alpha)
	fmt.Fprintf(buf, "\x1b[38;2;%d;%d;%dm", r, g, b)
}

func fadeSGRSetsForeground(params ansi.Params) bool {
	if len(params) == 0 {
		return true
	}
	for i := 0; i < len(params); {
		p := params[i].Param(0)
		if p == 0 || p == 39 {
			return true
		}
		if ci, n := fadeExtractColor(params, i, &FadeContext{}); n > 0 {
			if ci.prefix == "38" {
				return true
			}
			i += n
			continue
		}
		i++
	}
	return false
}

// ---------------------------------------------------------------------------
// SGR rewriting
// ---------------------------------------------------------------------------

// fadeColorInfo holds a resolved color extracted from SGR params.
type fadeColorInfo struct {
	r, g, b   float64
	prefix    string // output prefix: "38", "48", "58"
	useColons bool   // emit with colon separators (38:2:R:G:B)
	drop      bool   // discard this param (default bg/underline = already theme bg)
}

// fadeExtractColor checks if params[i] starts a color sequence. Returns the
// resolved color and number of params consumed. consumed=0 means not a color.
func fadeExtractColor(params ansi.Params, i int, fc *FadeContext) (ci fadeColorInfo, consumed int) {
	p := params[i].Param(0)

	// Basic and bright colors: single-param, map to 16-color palette.
	//   30-37  → fg (palette 0-7)      40-47  → bg (palette 0-7)
	//   90-97  → fg (palette 8-15)     100-107 → bg (palette 8-15)
	switch {
	case p >= 30 && p <= 37:
		r, g, b := basicColorRGB(p - 30)
		return fadeColorInfo{r: r, g: g, b: b, prefix: "38"}, 1
	case p >= 40 && p <= 47:
		r, g, b := basicColorRGB(p - 40)
		return fadeColorInfo{r: r, g: g, b: b, prefix: "48"}, 1
	case p >= 90 && p <= 97:
		r, g, b := basicColorRGB(p - 90 + 8)
		return fadeColorInfo{r: r, g: g, b: b, prefix: "38"}, 1
	case p >= 100 && p <= 107:
		r, g, b := basicColorRGB(p - 100 + 8)
		return fadeColorInfo{r: r, g: g, b: b, prefix: "48"}, 1
	}

	// Extended colors: 38 (fg), 48 (bg), 58 (underline) followed by sub-params.
	if p == 38 || p == 48 || p == 58 {
		return fadeExtractExtended(params, i, strconv.Itoa(p))
	}

	// Default fg (39): resolve from theme text-primary so it fades.
	if p == 39 {
		return fadeColorInfo{r: fc.defFgR, g: fc.defFgG, b: fc.defFgB, prefix: "38"}, 1
	}

	// Default background/underline resets must pass through. Dropping them
	// would leak a preceding explicit color into the rest of the pane.

	return fadeColorInfo{}, 0
}

// fadeExtractExtended parses X;2;R;G;B or X;5;N sub-params.
func fadeExtractExtended(params ansi.Params, i int, prefix string) (ci fadeColorInfo, consumed int) {
	if i+1 >= len(params) {
		return fadeColorInfo{}, 0
	}

	colons := params[i].HasMore()
	end := len(params)
	if colons {
		end = i + 1
		for end < len(params) && params[end-1].HasMore() {
			end++
		}
	}
	kind := params[i+1].Param(0)

	switch kind {
	case 2: // X;2;R;G;B
		remaining := end - i
		if remaining < 5 {
			return fadeColorInfo{}, 0
		}
		start := i + 2
		// ISO colon form may include an empty or explicit color-space slot.
		if colons && remaining == 6 {
			start++
		}
		r := float64(params[start].Param(0))
		g := float64(params[start+1].Param(0))
		b := float64(params[start+2].Param(0))
		return fadeColorInfo{r: r, g: g, b: b, prefix: prefix, useColons: colons}, start + 3 - i

	case 5: // X;5;N
		if i+2 >= end {
			return fadeColorInfo{}, 0
		}
		r, g, b := indexedColorRGB(params[i+2].Param(0))
		return fadeColorInfo{r: r, g: g, b: b, prefix: prefix, useColons: colons}, 3

	default:
		return fadeColorInfo{}, 0
	}
}

// fadeRewriteSGR rewrites an SGR sequence: every color is interpolated toward
// the theme background, everything else passes through unchanged.
func fadeRewriteSGR(buf *strings.Builder, params ansi.Params, alpha float64, fc *FadeContext) {
	if len(params) == 0 {
		r, g, b := fc.interpolate(fc.defFgR, fc.defFgG, fc.defFgB, alpha)
		fmt.Fprintf(buf, "\x1b[m\x1b[38;2;%d;%d;%dm", r, g, b)
		return
	}

	var parts []string
	i := 0
	for i < len(params) {
		p := params[i].Param(0)

		if p == 0 {
			if len(parts) > 0 {
				buf.WriteString("\x1b[")
				buf.WriteString(strings.Join(parts, ";"))
				buf.WriteByte('m')
				parts = parts[:0]
			}
			r, g, b := fc.interpolate(fc.defFgR, fc.defFgG, fc.defFgB, alpha)
			fmt.Fprintf(buf, "\x1b[0m\x1b[38;2;%d;%d;%dm", r, g, b)
			i++
			continue
		}

		if ci, n := fadeExtractColor(params, i, fc); n > 0 {
			if !ci.drop {
				ir, ig, ib := fc.interpolate(ci.r, ci.g, ci.b, alpha)
				parts = append(parts, fadeFmtColor(ci.prefix, ir, ig, ib, ci.useColons))
			}
			i += n
			continue
		}

		// Incomplete/unsupported extended colors remain opaque. Their channel
		// parameters (especially zero) are not independent SGR reset codes.
		if (p == 38 || p == 48 || p == 58) && !params[i].HasMore() {
			var raw []string
			for ; i < len(params); i++ {
				raw = append(raw, strconv.Itoa(params[i].Param(0)))
			}
			parts = append(parts, strings.Join(raw, ";"))
			continue
		}

		var part strings.Builder
		part.WriteString(strconv.Itoa(p))
		for params[i].HasMore() && i+1 < len(params) {
			i++
			part.WriteByte(':')
			part.WriteString(strconv.Itoa(params[i].Param(0)))
		}
		parts = append(parts, part.String())
		i++
	}

	if len(parts) > 0 {
		buf.WriteString("\x1b[")
		buf.WriteString(strings.Join(parts, ";"))
		buf.WriteByte('m')
	}
}

func fadeFmtColor(prefix string, r, g, b int, colons bool) string {
	if colons {
		return prefix + ":2:" + strconv.Itoa(r) + ":" + strconv.Itoa(g) + ":" + strconv.Itoa(b)
	}
	return prefix + ";2;" + strconv.Itoa(r) + ";" + strconv.Itoa(g) + ";" + strconv.Itoa(b)
}

// ---------------------------------------------------------------------------
// Color lookup tables
// ---------------------------------------------------------------------------

// parseHexRGB converts a CSS hex color to float64 RGB components (0-255).
func fadeParseHexRGB(hex string) (float64, float64, float64) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return 0, 0, 0
	}
	v, err := strconv.ParseUint(hex, 16, 24)
	if err != nil {
		return 0, 0, 0
	}
	return float64(v >> 16), float64((v >> 8) & 255), float64(v & 255)
}

// Standard 4-bit terminal colors (indices 0-15).
var basicColors = [16][3]float64{
	{0, 0, 0},       // 0  black
	{205, 0, 0},     // 1  red
	{0, 205, 0},     // 2  green
	{205, 205, 0},   // 3  yellow
	{0, 0, 238},     // 4  blue
	{205, 0, 205},   // 5  magenta
	{0, 205, 205},   // 6  cyan
	{229, 229, 229}, // 7  white
	{127, 127, 127}, // 8  bright black
	{255, 0, 0},     // 9  bright red
	{0, 255, 0},     // 10 bright green
	{255, 255, 0},   // 11 bright yellow
	{92, 92, 255},   // 12 bright blue
	{255, 0, 255},   // 13 bright magenta
	{0, 255, 255},   // 14 bright cyan
	{255, 255, 255}, // 15 bright white
}

func basicColorRGB(idx int) (float64, float64, float64) {
	if idx < 0 || idx >= 16 {
		return 0, 0, 0
	}
	c := basicColors[idx]
	return c[0], c[1], c[2]
}

func indexedColorRGB(idx int) (float64, float64, float64) {
	switch {
	case idx < 16:
		return basicColorRGB(idx)
	case idx < 232:
		// 6x6x6 color cube (indices 16-231)
		idx -= 16
		b := idx % 6
		idx /= 6
		g := idx % 6
		r := idx / 6
		return cubeComponent(r), cubeComponent(g), cubeComponent(b)
	case idx < 256:
		// Grayscale ramp (indices 232-255): 8, 18, 28 ... 238
		v := float64(8 + (idx-232)*10)
		return v, v, v
	default:
		return 0, 0, 0
	}
}

func cubeComponent(v int) float64 {
	if v == 0 {
		return 0
	}
	return float64(55 + v*40)
}
