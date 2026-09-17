package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestFormatTokens(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "500", FormatTokens(500))
	assert.Equal(t, "999", FormatTokens(999))
	assert.Equal(t, "1.0k", FormatTokens(1000))
	assert.Equal(t, "1.2k", FormatTokens(1234))
	assert.Equal(t, "1.0M", FormatTokens(1_000_000))
	assert.Equal(t, "2.5M", FormatTokens(2_500_000))
}

func TestComposeLineRightAligns(t *testing.T) {
	t.Parallel()
	out := ComposeLine("left", "right", 20)
	assert.Equal(t, 20, DisplayWidth(out))
	assert.GreaterOrEqual(t, len(out), len("left")+len("right"))
	assert.Contains(t, out, "left")
	assert.Contains(t, out, "right")
}

func TestComposeLineTruncatesLeft(t *testing.T) {
	t.Parallel()
	out := ComposeLine("a very long left side that does not fit", "right", 15)
	assert.LessOrEqual(t, DisplayWidth(out), 15)
	assert.Contains(t, out, "right")
}

func TestRenderContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    StatusModel
		want string
	}{
		{"before any usage", StatusModel{}, "0% · 0/0"},
		{"limit known before usage", StatusModel{ContextLimit: 200_000}, "0% · 0/200.0k"},
		{"usage within limit", StatusModel{ContextLength: 24_000, ContextLimit: 200_000}, "12% · 24.0k/200.0k"},
		{"usage over limit clamps the percentage", StatusModel{ContextLength: 15_000, ContextLimit: 10_000}, "100% · 15.0k/10.0k"},
		{"tokens without a known limit", StatusModel{Tokens: 1_200}, "1.2k tokens"},
		{"compacting keeps the token counts", StatusModel{ContextLength: 9_000, ContextLimit: 10_000, Compacting: true}, "compacting… · 9.0k/10.0k"},
		{"compacting without a known limit", StatusModel{Compacting: true}, "compacting…"},
		{"cost is appended when known", StatusModel{ContextLength: 5_000, ContextLimit: 10_000, Cost: 0.05, CostKnown: true}, "50% · 5.0k/10.0k · $0.05"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ansi.Strip(RenderContext(tt.d)))
		})
	}
}

func TestContextStyleEscalatesTowardsTheThreshold(t *testing.T) {
	t.Parallel()
	render := func(pct, threshold float64) string { return contextStyle(pct, threshold).Render("x") }

	assert.Equal(t, StMuted().Render("x"), render(0.5, 0))
	assert.Equal(t, StWarning().Render("x"), render(0.7, 0))
	assert.Equal(t, StError().Render("x"), render(0.95, 0))
	assert.Equal(t, StError().Render("x"), render(0.48, 0.5), "a configured threshold moves the bands")
}

func TestRenderStatusFitsWidth(t *testing.T) {
	t.Parallel()
	d := StatusModel{
		WorkingDir:    "/home/user/project",
		Branch:        "main",
		Agent:         "coder",
		Model:         "openai/gpt-5",
		Thinking:      "high",
		ContextLength: 24_000,
		ContextLimit:  200_000,
		Tokens:        24_000,
		Cost:          0.05,
		CostKnown:     true,
	}
	lines := RenderStatus(d, 80)
	assert.Len(t, lines, 2)
	assert.Contains(t, strings.Join(lines, "\n"), "$0.05")
	for _, l := range lines {
		assert.LessOrEqual(t, DisplayWidth(l), 80)
	}
}

func TestReasoningModesRenderAuthoritativeLabelsWithoutExtraRows(t *testing.T) {
	for _, mode := range []string{"auto", "default", "off", "adaptive", "effort", "tokens", "unknown", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			level := mode
			if mode == "adaptive" || mode == "effort" {
				level = "high"
			}
			if mode == "tokens" {
				level = "4096"
			}
			lines := RenderStatus(StatusModel{Model: "provider/raw", ModelName: "Friendly model", Provider: "provider", ThinkingMode: mode, ThinkingLevel: level}, 100)
			if len(lines) != 2 {
				t.Fatalf("footer grew: %d rows", len(lines))
			}
			if !strings.Contains(strings.Join(lines, ""), "provider("+level+")") {
				t.Fatalf("missing authoritative level: %v", lines)
			}
		})
	}
}

func TestPrimaryReasoningStatusKeepsFallbackTruthAndTwoRows(t *testing.T) {
	for _, primary := range []ThinkingStatus{
		{Mode: "effort", Level: "high"},
		{Mode: "unsupported"},
		{},
	} {
		level := primary.Level
		if level == "" {
			level = primary.Mode
		}
		if level == "" {
			level = "unknown"
		}
		status := StatusModel{Model: "p/fallback", ModelName: "Friendly fallback", Provider: "p", ThinkingMode: "effort", ThinkingLevel: "low", PrimaryThinking: &primary}
		lines := RenderStatus(status, 120)
		assert.Len(t, lines, 2)
		assert.Contains(t, strings.Join(lines, "\n"), "Friendly fallback")
		assert.Contains(t, strings.Join(lines, "\n"), "p(low)")
		assert.Contains(t, strings.Join(lines, "\n"), "(primary: "+level+")")
		for _, width := range []int{1, 12, 40} {
			lines = RenderStatus(status, width)
			assert.Len(t, lines, 2)
			for _, line := range lines {
				assert.LessOrEqual(t, DisplayWidth(line), width)
			}
		}
	}
}
