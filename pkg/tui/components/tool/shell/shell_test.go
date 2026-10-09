package shell

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestFormatShellOutput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{"empty", "", ""},
		{"only newlines", "\r\n\n\r", ""},
		{"normalize carriage returns", "one\r\ntwo\rthree\n\n", "one\ntwo\nthree"},
		{"preserve whitespace", "\n  one \t\n \n", "\n  one \t\n "},
		{"exact limit", strings.Repeat("line\n", 20), strings.TrimSuffix(strings.Repeat("line\n", 20), "\n")},
		{"overflow", "discard\n" + strings.Repeat("line\n", 20), "…\n" + strings.TrimSuffix(strings.Repeat("line\n", 20), "\n")},
		{"empty discarded line", "\n" + strings.Repeat("line\n", 20), "…\n" + strings.TrimSuffix(strings.Repeat("line\n", 20), "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatShellOutput(tc.output, 80))
		})
	}
}

func TestFormatShellOutputWrappingEquivalence(t *testing.T) {
	t.Parallel()

	patterns := []string{
		"",
		"short",
		strings.Repeat(" ", 30),
		"\t  indented\ttext  ",
		strings.Repeat("abcdefghij", 25),
		strings.Repeat("word boundary-", 20),
		strings.Repeat("界日本語", 30),
		strings.Repeat("e\u0301👩🏽‍💻🏳️‍🌈", 20),
		strings.Repeat("\x1b[31mred text\x1b[0m ", 20),
		strings.Repeat("\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ ", 10),
		"\x1b[31m" + strings.Repeat("ansi spans lines\n", 25) + "\x1b[0m",
		"invalid UTF-8: \xff\xfe" + strings.Repeat("界", 20),
		"\x1b[\n31m\n" + strings.Repeat("broken escape ", 10),
	}
	for pattern, line := range patterns {
		for _, count := range []int{1, 19, 20, 21, 45} {
			for _, separator := range []string{"\n", "\r\n", "\r"} {
				// Include differing final-line widths and trailing line endings.
				output := strings.Repeat(line+separator, count) + "tail 界 e\u0301" + separator + separator
				for _, width := range []int{-1, 0, 10, 11, 24, 80, 160} {
					t.Run(fmt.Sprintf("pattern%d/lines%d/separator%q/width%d", pattern, count, separator, width), func(t *testing.T) {
						assert.Equal(t, formatShellOutputBefore(output, width), formatShellOutput(output, width))
					})
				}
			}
		}
	}
}

func TestFormatShellOutputMixedLineWidths(t *testing.T) {
	t.Parallel()

	for _, count := range []int{19, 20, 21, 80} {
		var output strings.Builder
		for i := range count {
			fmt.Fprintf(&output, "line %03d: %s\n", i, strings.Repeat("界 e\u0301 \x1b[31mtext\x1b[0m ", i%7))
		}
		for _, width := range []int{10, 24, 80, 160} {
			t.Run(fmt.Sprintf("lines%d/width%d", count, width), func(t *testing.T) {
				assert.Equal(t, formatShellOutputBefore(output.String(), width), formatShellOutput(output.String(), width))
			})
		}
	}
}

// Preserve the previous formatter as a byte-for-byte oracle, including its
// rune-boundary wrapping of ANSI sequences and combining characters.
func formatShellOutputBefore(output string, width int) string {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.ReplaceAll(output, "\r", "\n")
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return ""
	}

	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 10)
	lines := toolcommon.WrapLines(output, availableWidth)
	if len(lines) > maxVisibleShellOutputLines {
		lines = append([]string{"…"}, lines[len(lines)-maxVisibleShellOutputLines:]...)
	}
	return strings.Join(lines, "\n")
}
