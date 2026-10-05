package message

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSubagentRepliesCollapseWithoutRenderingDeliveredBody(t *testing.T) {
	for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
		t.Run(string(origin), func(t *testing.T) {
			body := "<system_info>Delivered preview **literal**\n\tindented\nlast line</system_info>\n"
			input := session.UserMessage(body)
			input.InputOrigin, input.InputMode, input.SenderID = origin, "steer", "child-session"
			msg := types.Input(input)
			msg.InputReference = lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "abcde-node", Name: "Cafe\u0301 long worker 界", Agent: "worker", DisplayID: "abcde"}
			view := New(animation.NewRuntime(), msg, nil)
			assert.Equal(t, body, msg.ReceivedBody)
			for _, width := range []int{80, 28, 8, 4, 80} {
				view.SetSize(width, 100)
				out := view.View()
				assert.NotContains(t, ansi.Strip(out), "Delivered")
				lines := strings.Split(out, "\n")
				hits := 0
				for y, line := range lines {
					assert.LessOrEqual(t, ansi.StringWidth(line), width)
					for x := range width {
						if view.IsToggleAt(y, x) {
							assert.Equal(t, ">", ansi.Strip(ansi.Cut(line, x, x+1)))
							hits++
						}
					}
					assert.True(t, view.InputReferenceOnLine(y))
				}
				require.Equal(t, 1, hits)
				assert.False(t, view.InputReferenceOnLine(len(lines)))
			}
			view.SetExpanded(!view.IsExpanded())
			other := New(animation.NewRuntime(), msg, nil)
			assert.NotContains(t, other.View(), "Delivered", "expansion belongs to each view")
			for _, width := range []int{100, 90, 100} {
				view.SetSize(width, 100)
				view.InvalidateRenderCache()
				view.SetHovered(true)
				view.Finalize()
				out := ansi.Strip(view.View())
				if origin == session.InputOriginRuntime {
					assert.Contains(t, out, "v · report received")
				} else {
					assert.Contains(t, out, "v has replied")
				}
				for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
					assert.Contains(t, out, strings.ReplaceAll(line, "\t", "    "))
				}
				assert.False(t, view.InputReferenceOnLine(1), "expanded body is not navigation")
				assert.False(t, view.IsToggleAt(1, 0))
			}
			view.SetExpanded(!view.IsExpanded())
			msg.ReceivedBody = strings.Repeat("expensive hidden payload\n", 100000)
			assert.Nil(t, PrepareRender(view), "collapsed replies need no asynchronous body preparation")
			assert.Less(t, len(view.View()), 4096)
			assert.Nil(t, view.mdRenderer)
			assert.NotContains(t, New(animation.NewRuntime(), msg, nil).View(), "expensive")
		})
	}
}

func TestSubagentReplyGateExcludesInstructionsAndGenericNotices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin session.InputOrigin
		mode   string
		kind   lifecycle.InputReferenceKind
	}{
		{"delegation", session.InputOriginAgent, "turn", lifecycle.InputReferenceNode},
		{"parent steer", session.InputOriginAgent, "steer", lifecycle.InputReferenceParent},
		{"unresolved", session.InputOriginAgent, "steer", lifecycle.InputReferenceUnknown},
		{"user", session.InputOriginUser, "steer", lifecycle.InputReferenceNode},
		{"generic runtime", session.InputOriginRuntime, "steer", lifecycle.InputReferenceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := session.UserMessage("unchanged body")
			input.InputOrigin, input.InputMode, input.SenderID = tc.origin, tc.mode, "sender"
			msg := types.Input(input)
			msg.InputReference = lifecycle.InputReference{Kind: tc.kind, Name: "sender"}
			assert.False(t, msg.IsSubagentReply())
			view := New(animation.NewRuntime(), msg, nil)
			view.SetExpanded(true)
			out := view.View()
			assert.NotContains(t, out, "has replied")
			if tc.origin == session.InputOriginRuntime {
				assert.NotContains(t, out, "unchanged body")
			} else {
				assert.Contains(t, out, "unchanged body")
			}
		})
	}
	generic := session.UserMessage("model-only envelope")
	generic.InputOrigin, generic.InputMode = session.InputOriginRuntime, "steer"
	assert.Empty(t, types.Input(generic).ReceivedBody)
}
