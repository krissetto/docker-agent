package message

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func assertWithinFull(t *testing.T, v *messageModel, width int) {
	t.Helper()
	parts, ok := v.RenderedSegments(width)
	require.True(t, ok)
	lines := append(append(append([]string{}, parts.Header...), parts.Stable...), parts.Tail...)
	style := styles.AssistantMessageStyle
	inner := width - style.GetHorizontalFrameSize()
	body, blocks, err := markdown.NewFastRenderer(inner).RenderWithCodeBlocks(v.message.Content)
	require.NoError(t, err)
	prefix := v.senderPrefix("root")
	want := prefix + style.Width(width).Render(actionRow(inner, v.hovered, types.MessageCopyLabel)+"\n"+body)
	require.Equal(t, uv.NewStyledString(want).Lines(ansi.GraphemeWidth), uv.NewStyledString(strings.Join(lines, "\n")).Lines(ansi.GraphemeWidth), "input %q width %d", v.message.Content, width)
	for i := range blocks {
		blocks[i].Line += strings.Count(prefix, "\n") + 1
	}
	require.Equal(t, blocks, v.CodeBlocks())
}
func TestMutableTailEveryChunk(t *testing.T) {
	for _, input := range []string{
		"- first **bold** and `code`\n- [link](https://example.com) 界 e\u0301 👩‍💻\n- tail\n",
		"- first\n  continuation\n- next\n", "- first\n\n- next\n", "- first\n  ```go\n  /* multiline\n  comment */\n  ```\n- next\n",
		"1. first\n10. second\n100. third\n", "| a | b |\n| - | - |\n| verywide | c |\n", "paragraph **open\nstyle** and link [x](https://example.com)",
		"- [ ] task\n- [x] done\n- **open\n- closes**\n", "- foo\n-\n- bar", "- foo\n---\n- bar",
	} {
		for _, width := range []int{18, 47, 100} {
			v := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
			for i := range len(input) {
				v.AppendContent(input[i : i+1])
				assertWithinFull(t, v, width)
			}
			v.Finalize()
			require.Empty(t, v.streamLines.tail.input)
			assertWithinFull(t, v, width)
		}
	}
}
func TestMutableTailInvalidation(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	v := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", "- first **bold**\n- second"), nil)
	for _, width := range []int{90, 23, 90} {
		assertWithinFull(t, v, width)
	}
	theme := *original
	theme.Colors.TextPrimary = "#123456"
	styles.ApplyTheme(&theme)
	assertWithinFull(t, v, 90)
	v.SetMessage(types.Agent(types.MessageTypeAssistant, "root", "- edited first\n- new second"))
	assertWithinFull(t, v, 90)
	v.SetHovered(true)
	assertWithinFull(t, v, 90)
}

func TestTailLineCachePreservesCrossLineANSI(t *testing.T) {
	for _, content := range []string{"\x1b[31mred\ncontinued\x1b[0m", "\x1b]8;;https://example.com\alink\ncontinued\x1b]8;;\a", "one\n\nthree", "\x1b[31mred\x1b[0m\nplain"} {
		c := assistantTailLines{}
		for _, width := range []int{20, 60} {
			c = assistantTailLines{}
			got := strings.Join(c.render(styles.AssistantMessageStyle, width, content), "\n")
			want := strings.Join(styledAssistantLines(styles.AssistantMessageStyle, width, content), "\n")
			require.Equal(t, uv.NewStyledString(want).Lines(ansi.GraphemeWidth), uv.NewStyledString(got).Lines(ansi.GraphemeWidth), "content %q width %d got %q want %q", content, width, got, want)
		}
	}
}

func TestMutableTailRetainsOnlyCurrentLines(t *testing.T) {
	view := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", "- one\n- two\n- three"), nil)
	assertWithinFull(t, view, 80)
	view.SetMessage(types.Agent(types.MessageTypeAssistant, "root", "- replaced"))
	assertWithinFull(t, view, 80)
	require.Len(t, view.streamLines.tail.input, 1)
	require.Len(t, view.streamLines.tail.rendered, 1)
	view.Finalize()
	require.Empty(t, view.streamLines.tail.input)
	require.Empty(t, view.streamLines.tail.rendered)
}

func FuzzMutableTailMatchesFullRenderer(f *testing.F) {
	for _, input := range []string{"- one\n- two", "```go\nvar x = `raw\nstring`\n```", "| a | b |\n| - | - |\n| long | value |", "paragraph **bold** [link](https://example.com)"} {
		f.Add(input, uint8(31))
	}
	f.Fuzz(func(t *testing.T, input string, size uint8) {
		if len(input) > 2048 || strings.Contains(input, "![") {
			t.Skip()
		}
		v := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
		width := int(size)%80 + 12
		for start := 0; start < len(input); start += 7 {
			v.AppendContent(input[start:min(start+7, len(input))])
			if strings.TrimSpace(v.message.Content) == "" {
				continue
			}
			body, _, err := markdown.NewFastRenderer(width - styles.AssistantMessageStyle.GetHorizontalFrameSize()).RenderWithCodeBlocks(v.message.Content)
			require.NoError(t, err)
			if body == "" {
				continue
			}
			assertWithinFull(t, v, width)
		}
	})
}
