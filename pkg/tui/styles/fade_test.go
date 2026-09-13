package styles

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFadeContext() *FadeContext {
	// Use a known bg/fg for deterministic tests.
	// bg=#1C1C22 (dark), fg=#E0E0E3 (light) — matches the default theme.
	fc := NewFadeContextRGB(28, 28, 34, 224, 224, 227)
	return &fc
}

// --- fadeParseHexRGB ---

func TestFadeParseHexRGB(t *testing.T) {
	tests := []struct {
		name    string
		hex     string
		r, g, b float64
	}{
		{"black", "#000000", 0, 0, 0},
		{"white", "#FFFFFF", 255, 255, 255},
		{"red", "#FF0000", 255, 0, 0},
		{"no hash", "1C1C22", 28, 28, 34},
		{"short form", "#FFF", 255, 255, 255},
		{"short form red", "#F00", 255, 0, 0},
		{"invalid", "XY", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, g, b := fadeParseHexRGB(tt.hex)
			assert.InDelta(t, tt.r, r, 0.01)
			assert.InDelta(t, tt.g, g, 0.01)
			assert.InDelta(t, tt.b, b, 0.01)
		})
	}
}

// --- FadeContext.interpolate ---

func TestFadeContext_Interpolate(t *testing.T) {
	fc := NewFadeContextRGB(0, 0, 0, 200, 200, 200)

	r, g, b := fc.interpolate(200, 0, 0, 0.5)
	assert.Equal(t, 100, r)
	assert.Equal(t, 0, g)
	assert.Equal(t, 0, b)

	r, g, b = fc.interpolate(200, 0, 0, 0.0)
	assert.Equal(t, 0, r)
	assert.Equal(t, 0, g)
	assert.Equal(t, 0, b)

	r, g, b = fc.interpolate(200, 0, 0, 1.0)
	assert.Equal(t, 200, r)
	assert.Equal(t, 0, g)
	assert.Equal(t, 0, b)
}

// --- NewFadeContextRGB ---

func TestNewFadeContextRGB(t *testing.T) {
	fc := NewFadeContextRGB(10, 20, 30, 100, 150, 200)
	assert.InDelta(t, 10, fc.bgR, 0.01)
	assert.InDelta(t, 20, fc.bgG, 0.01)
	assert.InDelta(t, 30, fc.bgB, 0.01)
	assert.InDelta(t, 100, fc.defFgR, 0.01)
	assert.InDelta(t, 150, fc.defFgG, 0.01)
	assert.InDelta(t, 200, fc.defFgB, 0.01)
}

// --- color utilities ---

func TestIndexedColorRGB(t *testing.T) {
	r, g, b := indexedColorRGB(1) // red
	assert.InDelta(t, 205, r, 0.01)
	assert.InDelta(t, 0, g, 0.01)
	assert.InDelta(t, 0, b, 0.01)

	r, g, b = indexedColorRGB(196) // 6x6x6 cube bright red
	assert.InDelta(t, 255, r, 0.01)
	assert.InDelta(t, 0, g, 0.01)
	assert.InDelta(t, 0, b, 0.01)

	r, g, b = indexedColorRGB(232) // grayscale 8
	assert.InDelta(t, 8, r, 0.01)
	assert.InDelta(t, 8, g, 0.01)
	assert.InDelta(t, 8, b, 0.01)
}

func TestBasicColorRGB_OutOfRange(t *testing.T) {
	r, g, b := basicColorRGB(-1)
	assert.InDelta(t, 0, r, 0.01)
	assert.InDelta(t, 0, g, 0.01)
	assert.InDelta(t, 0, b, 0.01)

	r, g, b = basicColorRGB(16)
	assert.InDelta(t, 0, r, 0.01)
	assert.InDelta(t, 0, g, 0.01)
	assert.InDelta(t, 0, b, 0.01)
}

func TestIndexedColorRGB_GrayscaleRamp(t *testing.T) {
	// Index 232 = 8, index 255 = 238
	r, g, b := indexedColorRGB(255)
	expected := float64(8 + (255-232)*10)
	assert.InDelta(t, expected, r, 0.01)
	assert.InDelta(t, expected, g, 0.01)
	assert.InDelta(t, expected, b, 0.01)
}

func TestIndexedColorRGB_OutOfRange(t *testing.T) {
	r, g, b := indexedColorRGB(256)
	assert.InDelta(t, 0, r, 0.01)
	assert.InDelta(t, 0, g, 0.01)
	assert.InDelta(t, 0, b, 0.01)
}

// --- FadeLineCtx: color preservation ---

func TestFadeLineCtx_EmptyLinesUnchanged(t *testing.T) {
	fc := testFadeContext()
	blank := "       "
	result := FadeLineCtx(blank, 0.5, fc)
	assert.Equal(t, blank, result, "whitespace-only lines should pass through unchanged")
}

func TestFadeLineCtx_PlainText(t *testing.T) {
	fc := testFadeContext()
	result := FadeLineCtx("hello world", 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "hello world", stripped, "stripped text should be preserved")
	assert.Contains(t, result, "\x1b[38;2;", "result should contain 24-bit fg sequence")
}

func TestFadeLineCtx_PreservesTextContent(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[31mred text\x1b[0m normal"
	result := FadeLineCtx(styled, 0.75, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "red text normal", stripped, "all text content must be preserved")
}

func TestFadeLineCtx_PreservesBasicFgColor(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[31mred\x1b[m"

	result50 := FadeLineCtx(styled, 0.5, fc)
	result100 := FadeLineCtx(styled, 1.0, fc)

	assert.Contains(t, result50, "\x1b[38;2;")
	assert.Contains(t, result100, "\x1b[38;2;")
	assert.NotEqual(t, result50, result100, "different alphas should produce different colors")
}

func TestFadeLineCtx_Preserves24BitFgColor(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[38;2;0;255;0mgreen\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "green", stripped)

	r, g, b := extractFirstFgRGB(result)
	assert.Greater(t, g, r, "green channel should dominate for green input")
	assert.Greater(t, g, b, "green channel should dominate for green input")
}

func TestFadeLineCtx_Preserves256ColorFg(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[38;5;196mred\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "red", stripped)

	assert.Contains(t, result, "\x1b[38;2;")
	r, _, b := extractFirstFgRGB(result)
	assert.Greater(t, r, b, "red channel should dominate for red input")
}

func TestFadeLineCtx_PreservesBrightFgColor(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[92mbright green\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "bright green", stripped)

	r, g, b := extractFirstFgRGB(result)
	assert.Greater(t, g, r, "green channel should dominate for bright green")
	assert.Greater(t, g, b, "green channel should dominate for bright green")
}

func TestFadeLineCtx_PreservesNonFgAttributes(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[1;31mbold red\x1b[m"

	result := FadeLineCtx(styled, 0.75, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "bold red", stripped)

	assert.Contains(t, result, "1;", "bold attribute should be preserved")
}

func TestFadeLineCtx_AttributesOnlyApplyFadedDefaultForegroundBeforeText(t *testing.T) {
	fc := testFadeContext()
	for _, styled := range []string{"\x1b[1mbold\x1b[m", "\x1b[3mitalic\x1b[m"} {
		result := FadeLineCtx(styled, 0.5, fc)
		attribute := strings.Index(result, "m")
		foreground := strings.Index(result, "\x1b[38;2;126;126;130m")
		text := strings.Index(result, ansi.Strip(styled))

		assert.GreaterOrEqual(t, attribute, 0)
		assert.Greater(t, foreground, attribute, "attribute should be preserved before injected foreground")
		assert.Greater(t, text, foreground, "faded default foreground must be applied before text")
	}
}

func TestFadeLineCtx_PreservesBackgroundColor(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[31;44mtext\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "text", stripped)

	assert.Contains(t, result, "48;2;", "basic bg should be rewritten to 24-bit")
	assert.Contains(t, result, "38;2;", "fg should be interpolated")
}

func TestFadeLineCtx_MultipleColorSegments(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[31mred \x1b[32mgreen \x1b[34mblue\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "red green blue", stripped)

	count := strings.Count(result, "38;2;")
	assert.GreaterOrEqual(t, count, 3, "each color segment should get its own interpolated color")
}

func TestFadeLineCtx_ResetInjectsDefaultFg(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[31mred\x1b[0m normal"

	result := FadeLineCtx(styled, 0.5, fc)

	count := strings.Count(result, "38;2;")
	assert.GreaterOrEqual(t, count, 2, "reset should inject faded default fg")
}

func TestFadeLineCtx_ColonSeparatedParams(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[38:2:0:200:100mtext\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "text", stripped)
	assert.Contains(t, result, "38:2:")
}

func TestFadeLineCtx_BackgroundColorInterpolated(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[48;2;30;60;30;38;2;0;200;0mAdded\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "Added", stripped)

	assert.NotContains(t, result, "48;2;30;60;30", "bg color should be interpolated, not raw")
	assert.Contains(t, result, "48;2;", "bg should still use 24-bit color")

	r, g, b := extractFirstFgRGB(result)
	assert.Greater(t, g, r, "green channel should dominate for green fg")
	assert.Greater(t, g, b, "green channel should dominate for green fg")
}

func TestFadeLineCtx_BackgroundColorDoesNotCorruptFg(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[48;2;60;30;30;38;2;200;80;80mRemoved\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "Removed", stripped)

	assert.NotContains(t, result, "48;2;60;30;30", "bg should be interpolated")
	assert.Contains(t, result, "48;2;", "bg should still use 24-bit color")

	r, g, b := extractFirstFgRGB(result)
	assert.Greater(t, r, g, "red channel should dominate for red fg")
	assert.Greater(t, r, b, "red channel should dominate for red fg")
}

func TestFadeLineCtx_FgAfterBgWithBold(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[1;48;2;40;20;20;38;2;200;100;100mtext\x1b[m"

	result := FadeLineCtx(styled, 0.75, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "text", stripped)

	assert.Contains(t, result, "1;")
	assert.Contains(t, result, "48;2;")
}

func TestFadeLineCtx_256ColorBackground(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[48;5;22;38;2;0;200;0mtext\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "text", stripped)

	assert.NotContains(t, result, "48;5;22", "256-color bg should be rewritten to 24-bit")
	assert.Contains(t, result, "48;2;", "bg should be 24-bit after interpolation")

	r, g, _ := extractFirstFgRGB(result)
	assert.Greater(t, g, r, "green channel should dominate")
}

func TestFadeLineCtx_FgRGBFollowedByBold(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[38;2;0;200;0;1mgreen bold\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "green bold", stripped)

	r, g, b := extractFirstFgRGB(result)
	assert.Greater(t, g, r, "green channel should dominate")
	assert.Greater(t, g, b, "green channel should dominate")

	assert.Contains(t, result, "1m")
}

func TestFadeLineCtx_BasicBgColorInterpolated(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[44;37mtext\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "text", stripped)

	assert.Contains(t, result, "48;2;", "basic bg should be rewritten to 24-bit")
}

func TestFadeLineCtx_BgFullAlphaPreservesColor(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[48;2;100;150;200mtext\x1b[m"

	result := FadeLineCtx(styled, 1.0, fc)

	idx := strings.Index(result, "48;2;")
	require.GreaterOrEqual(t, idx, 0, "should contain 48;2;")
	var r, g, b int
	_, _ = fmt.Sscanf(result[idx+len("48;2;"):], "%d;%d;%d", &r, &g, &b)

	assert.Equal(t, 100, r)
	assert.Equal(t, 150, g)
	assert.Equal(t, 200, b)
}

func TestFadeLineCtx_UnderlineColorInterpolated(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[4;58;2;255;0;0munderlined\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "underlined", stripped)

	assert.NotContains(t, result, "58;2;255;0;0", "underline color should be interpolated")
	assert.Contains(t, result, "58;2;", "underline color should still use 24-bit")
	assert.Contains(t, result, "4;")
}

func TestFadeLineCtx_DefaultUnderlineColorDropped(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b[38;2;200;100;50;59mtext\x1b[m"

	result := FadeLineCtx(styled, 0.5, fc)
	stripped := ansi.Strip(result)
	assert.Equal(t, "text", stripped)
	assert.NotContains(t, result, "59")
}

// --- test helpers ---

func extractFirstFgRGB(s string) (r, g, b int) {
	for _, sep := range []string{"38;2;", "38:2:"} {
		_, after0, ok := strings.Cut(s, sep)
		if !ok {
			continue
		}
		after := after0
		var delim byte = ';'
		if sep == "38:2:" {
			delim = ':'
		}
		_, _ = fmt.Sscanf(after, "%d"+string(delim)+"%d"+string(delim)+"%d", &r, &g, &b)
		return r, g, b
	}
	return 0, 0, 0
}

func TestFadeLineCtx_PreservesHyperlinksWideTextAndColonAttributes(t *testing.T) {
	fc := testFadeContext()
	styled := "\x1b]8;;https://example.com\x1b\\\x1b[4:3;38:2::100:200:50m界🙂\x1b]8;;\x1b\\"
	result := FadeLineCtx(styled, 0.5, fc)
	assert.Equal(t, ansi.Strip(styled), ansi.Strip(result))
	assert.Equal(t, ansi.StringWidth(styled), ansi.StringWidth(result))
	assert.Contains(t, result, "4:3;38:2:64:114:42")
	assert.Contains(t, result, "\x1b]8;;https://example.com\x1b\\")
	assert.Contains(t, result, "\x1b]8;;\x1b\\")
	assert.True(t, strings.HasSuffix(result, "\x1b[m"), "fade must not leak into scrollbar or outer padding")
}

func BenchmarkFadeLineCtx(b *testing.B) {
	fc := testFadeContext()
	for name, line := range map[string]string{
		"plain": strings.Repeat("text 界 ", 16),
		"ansi":  strings.Repeat("\x1b[1;38;2;100;180;220;48;2;30;40;50mtext 界\x1b[m ", 16),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = FadeLineCtx(line, 0.75, fc)
			}
		})
	}
}
