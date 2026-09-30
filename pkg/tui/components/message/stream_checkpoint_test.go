package message

import (
	"fmt"
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

func TestStreamCheckpointSegmentsMatchUncachedRender(t *testing.T) {
	const input = "### 界 e\u0301 👩‍💻\n- **bold** [link](https://example.com) `code`\n# Next\n| a | b |\n| - | - |\n| c | d |\n\n```go\nvar x = 1\n```\n# Final\nDone."
	for _, width := range []int{24, 80} {
		view := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
		for i := range len(input) {
			view.AppendContent(input[i : i+1])
			segments, ok := view.RenderedSegments(width)
			require.True(t, ok)
			lines := append(append(append([]string{}, segments.Header...), segments.Stable...), segments.Tail...)
			fresh := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", input[:i+1]), nil)
			rendered, blocks, err := markdown.NewFastRenderer(width - styles.AssistantMessageStyle.GetHorizontalFrameSize()).RenderWithCodeBlocks(input[:i+1])
			require.NoError(t, err)
			expected := fresh.senderPrefix("root") + styles.AssistantMessageStyle.Width(width).Render(actionRow(width-styles.AssistantMessageStyle.GetHorizontalFrameSize(), false, types.MessageCopyLabel)+"\n"+rendered)
			for j := range blocks {
				blocks[j].Line += strings.Count(fresh.senderPrefix("root"), "\n") + 1
			}
			require.Equal(t, blocks, view.CodeBlocks())
			require.Equal(t, input[:i+1], view.message.Content)
			// Independent segments may emit fewer redundant SGR resets than one big
			// Lipgloss render; compare terminal cells including styles and hyperlinks.
			require.Equal(t, uv.NewStyledString(expected).Lines(ansi.GraphemeWidth), uv.NewStyledString(strings.Join(lines, "\n")).Lines(ansi.GraphemeWidth), "byte %d width %d", i, width)
		}
		view.Finalize()
		require.False(t, view.HasLiveRenderState())
		fresh := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", input), nil)
		require.Equal(t, fresh.render(width), view.Render(width))
		require.False(t, view.HasLiveRenderState())
	}
}

func BenchmarkStreamingAdjacentBlocks(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			chunks := make([]string, count)
			total := 0
			for i := range count {
				chunks[i] = fmt.Sprintf("\n### Item %d\n- deterministic **markdown** transcript with `code` and history context.\n", i)
				total += len(chunks[i])
			}
			b.SetBytes(int64(total))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				view := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
				for _, chunk := range chunks {
					view.AppendContent(chunk)
					view.RenderedSegments(120)
				}
				view.Finalize()
			}
		})
	}
}
