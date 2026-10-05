package subagenttool

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSendDisclosurePreservesRequestLiveReplayAndError(t *testing.T) {
	for _, to := range []string{"parent", "abcde-child"} {
		for _, status := range []types.ToolStatus{types.ToolStatusRunning, types.ToolStatusCompleted, types.ToolStatusError} {
			for _, replay := range []bool{false, true} {
				msg := testMessage("send_message", `{"to":"`+to+`","message":"Literal **request**\\nsecond line"}`, `Message delivered to subagent "worker" (abcde-child).`, status)
				if replay {
					msg.Content, msg.ToolResult = msg.ToolResult.Output, nil
				}
				ar := animation.NewRuntime()
				view := NewSend(ar, msg, nil, nil).(*sendModel)
				require.NotContains(t, view.View(), "Literal")
				for _, width := range []int{80, 28, 8, 4, 80} {
					view.SetSize(width, 0)
					hits := 0
					for y, line := range strings.Split(view.View(), "\n") {
						require.LessOrEqual(t, ansi.StringWidth(line), width)
						for x := range width {
							if view.IsToggleAt(y, x) {
								hits++
								require.Equal(t, ">", ansi.Strip(ansi.Cut(line, x, x+1)))
							}
						}
					}
					require.Equal(t, 1, hits)
				}
				view.SetExpanded(true)
				body := ansi.Strip(view.View())
				require.Contains(t, body, "Literal **request**")
				require.Equal(t, 1, strings.Count(body, "Literal"))
				require.Contains(t, body, "━", "expanded body uses agent-input border")
				if to != "parent" {
					require.Equal(t, 1, strings.Count(body, "worker (abcde)"), "one recipient identity")
					require.Contains(t, body, "worker (abcde) v")
				}
				old := view
				view = NewSend(ar, msg, nil, nil).(*sendModel)
				animation.StopView(old)
				// Tool status owners preserve view-local expansion across replacement.
				view.SetExpanded(old.IsExpanded())
				require.Contains(t, view.View(), "Literal")
				view.Toggle()
				require.True(t, view.NeedsTick())
				view.StopAnimation()
				require.Zero(t, ar.ActiveCount())
				require.NotContains(t, view.View(), "Literal")
			}
		}
	}
}
