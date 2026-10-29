package reasoningblock

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// legacyReasoningPreview is the previous full-document algorithm, kept only as
// an independent output oracle. It does not read the new compact cache.
func legacyReasoningPreview(t *testing.T, m *Model) (string, bool, bool) {
	t.Helper()
	if m.Reasoning() == "" {
		return "", false, len(m.toolEntries) > 0
	}
	rendered, err := markdown.NewRendererWithoutCopyIcon(m.contentWidth()).Render(m.Reasoning())
	require.NoError(t, err)
	all := strings.Split(strings.TrimRight(ansi.Strip(rendered), "\n\r\t "), "\n")
	var lines []string
	for _, line := range all {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	truncated := len(lines) > previewLines
	lines = lines[max(0, len(lines)-previewLines):]
	var styled []string
	for _, line := range lines {
		styled = append(styled, styles.MutedStyle.Italic(true).Render(line))
	}
	preview := m.messageStyle().Render(strings.Join(styled, "\n"))
	return preview, truncated, len(m.toolEntries) > 0 || len(all) > previewLines
}

func TestReasoningPreviewOwnsOnlyDisplayedLinesWithExactLegacyOutput(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, content := range []string{
		"",
		" \n\n ",
		"<!-- invisible -->",
		"short λ界 👩‍💻",
		"first\n\nsecond\n\nthird",
		strings.Repeat("paragraph **bold** λ界 👩‍💻\n\n```go\nvalue := 42\n```\n\n", 80) + "last\n\nend",
		"\x1b[31mred\x1b[0m\n\n> quote\n\n- one\n- two\n- three\n- four",
	} {
		for _, width := range []int{12, 40, 120} {
			t.Run(fmt.Sprintf("bytes%d-width%d", len(content), width), func(t *testing.T) {
				m := New(animation.NewRuntime(), "memory", "root", service.StaticSessionState{})
				m.SetReasoning(content)
				m.SetSize(width, 30)
				check := func() {
					want, truncated, extra := legacyReasoningPreview(t, m)
					got, gotTruncated := m.renderReasoningPreviewWithTruncationInfo()
					require.False(t, containsFaintSGR(got), "reasoning prose relies on its muted semantic style, not terminal faint")
					require.Equal(t, want, got)
					require.Equal(t, truncated, gotTruncated)
					require.Equal(t, extra, m.hasExtraContent())
					require.LessOrEqual(t, len(m.cache.lines), previewLines)
					require.Equal(t, content, m.Reasoning())
				}
				check()
				collapsed := m.View()
				m.Toggle()
				expanded := m.View()
				require.NotEmpty(t, expanded)
				m.Toggle()
				require.Equal(t, collapsed, m.View())
				check()
				m.SetSize(width+7, 30)
				check()
				oldCache := m.cache
				theme := *original
				theme.Colors.TextMuted = "#123456"
				styles.ApplyTheme(&theme)
				check()
				require.NotSame(t, oldCache, m.cache)
			})
		}
	}
}

func containsFaintSGR(rendered string) bool {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var state byte
	for rendered != "" {
		seq, _, n, next := ansi.DecodeSequence(rendered, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			params := p.Params()
			for i := 0; i < len(params); i++ {
				code := params[i].Param(0)
				switch code {
				case 2:
					return true
				case 38, 48, 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						i += consumed - 1
					}
				}
			}
		}
		state, rendered = next, rendered[n:]
	}
	return false
}
