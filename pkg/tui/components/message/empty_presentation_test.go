package message

import (
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestWhitespaceAssistantHasNoPresentation(t *testing.T) {
	for _, text := range []string{"", " ", "\t", "\n", " \t\n\r"} {
		t.Run(text, func(t *testing.T) {
			ar := animation.NewRuntime()
			defer ar.Stop()
			msg := &types.Message{Type: types.MessageTypeAssistant, Content: text}
			view := New(ar, msg, nil)
			view.SetSize(80, 20)
			for _, selected := range []bool{false, true} {
				view.SetSelected(selected)
				view.SetHovered(true)
				require.Empty(t, view.View())
				require.Zero(t, view.Height(80))
				segments, ok := view.RenderedSegments(80)
				if ok {
					require.Empty(t, segments.Header)
					require.Empty(t, segments.Stable)
					require.Empty(t, segments.Tail)
				}
			}
			require.Equal(t, text, msg.Content)
			view.AppendContent("\tindented prose")
			require.Equal(t, text+"\tindented prose", msg.Content)
			require.NotEmpty(t, view.View())
		})
	}
}
