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

func TestRenderComposite(t *testing.T) {
	t.Parallel()
	parent := lipgloss.NewStyle().Background(lipgloss.Color("#0000FF")).Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
	childStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000"))
	output := RenderComposite(parent, childStyle.Render("Hello")+" World")
	assert.Contains(t, output, "World")
	assert.Contains(t, output, "\x1b[1;")

	parts := strings.Split(output, resetFull)
	if len(parts) >= 2 {
		colorOnly := parent.UnsetBold().UnsetPadding().UnsetMargins().UnsetWidth().UnsetHeight()
		expectedSeq := strings.TrimSuffix(strings.TrimSuffix(colorOnly.Render(""), resetFull), resetShort)
		assert.True(t, strings.HasPrefix(parts[1], expectedSeq))
		fullSeq := strings.TrimSuffix(strings.TrimSuffix(parent.UnsetPadding().UnsetMargins().UnsetWidth().UnsetHeight().Render(""), resetFull), resetShort)
		assert.NotEqual(t, expectedSeq, fullSeq)
	}
}

func TestRenderComposite_FastPath(t *testing.T) {
	t.Parallel()
	parent := lipgloss.NewStyle().Background(lipgloss.Color("#0000FF")).Foreground(lipgloss.Color("#FFFFFF"))
	output := RenderComposite(parent, "Hello World")
	assert.Contains(t, output, "Hello World")
	assert.Contains(t, output, "\x1b[")
}

func TestRenderCompositeRestoresBackgroundAcrossOutputResets(t *testing.T) {
	parent := lipgloss.NewStyle().Foreground(lipgloss.Color("#F0F0F0")).Background(lipgloss.Color("#243647"))
	content := "before \x1b[38;2;245;177;66mamber\x1b[0m middle \x1b[48;2;9;8;7mcolored\x1b[49m after"
	rendered := RenderComposite(parent, content)
	for _, word := range []string{"before", "amber", "middle", "colored", "after"} {
		start := strings.Index(rendered, word)
		col := ansi.StringWidth(ansi.Strip(rendered[:start])) + 1
		require.Equal(t, color.NRGBAModel.Convert(lipgloss.Color("#243647")), color.NRGBAModel.Convert(sgrBackgroundAt(rendered, col)), "background at %q", word)
	}
	require.Contains(t, rendered, "38;2;245;177;66")
	require.Contains(t, rendered, "48;2;9;8;7")
}

func TestRenderCompositeCardBodyHasContinuousBackground(t *testing.T) {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#E0E0E0")).Background(lipgloss.Color("#354657")).Width(24)
	content := "heading\ncmd \x1b[38;2;200;150;20mtruecolor\x1b[0m\nout \x1b[48;2;2;3;4mreset\x1b[49m after"
	rows := strings.Split(RenderComposite(style, content), "\n")
	require.Len(t, rows, 3)
	for row, line := range rows {
		require.Equal(t, 24, ansi.StringWidth(line), "row=%d", row)
		for col := 1; col < 24; col++ {
			require.Equal(t, color.NRGBAModel.Convert(lipgloss.Color("#354657")), color.NRGBAModel.Convert(sgrBackgroundAt(line, col)), "row=%d col=%d", row, col)
		}
	}
}

func sgrBackgroundAt(line string, col int) color.Color {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	var background color.Color
	for x, rest := 0, line; rest != ""; {
		sequence, width, consumed, next := ansi.DecodeSequence(rest, state, parser)
		if consumed == 0 {
			break
		}
		if ansi.HasCsiPrefix(sequence) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				background = nil
			}
			for i := 0; i < len(params); i++ {
				switch params[i].Param(0) {
				case 0, 49:
					background = nil
				case 48:
					var parsed color.Color
					read := ansi.ReadStyleColor(params[i:], &parsed)
					if parsed != nil {
						background = parsed
					}
					i += max(0, read-1)
				}
			}
		}
		if width > 0 && x+width > col {
			return background
		}
		x += width
		state, rest = next, rest[consumed:]
	}
	return background
}

func wordAt(line string, col int, word string) bool {
	plain := ansi.Strip(line)
	start := strings.Index(plain, word)
	return start >= 0 && col >= start && col < start+len(word)
}
