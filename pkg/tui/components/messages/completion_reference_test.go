package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestCompletionReferenceRecoversOnlyExactRuntimeHeader(t *testing.T) {
	const first = "abcde-full-first"
	const second = "abcde-full-second"
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: first, SessionID: "current-first-session", Agent: "worker", Name: "duplicate name"}},
		{Node: subagent.Node{ID: second, SessionID: "current-second-session", Agent: "worker", Name: "duplicate name"}},
	}}
	for _, tc := range []struct {
		name, body, sender string
		origin             session.InputOrigin
		want               string
	}{
		{"finished", fmt.Sprintf("Subagent %q (%s) finished its turn. Full response: %q", `old "quoted" name`, first, "unchanged body"), "old-session", session.InputOriginRuntime, first},
		{"failed", fmt.Sprintf("Subagent %q (%s) failed. Error: %q", "name", second, "raw failure"), "old-session", session.InputOriginRuntime, second},
		{"typed sender wins", fmt.Sprintf("Subagent %q (%s) finished its turn.", "name", second), "current-first-session", session.InputOriginRuntime, first},
		{"short collision", `Subagent "duplicate name" (abcde) finished its turn.`, "old-session", session.InputOriginRuntime, ""},
		{"session not node", `Subagent "duplicate name" (current-first-session) finished its turn.`, "old-session", session.InputOriginRuntime, ""},
		{"unanchored", `prose Subagent "duplicate name" (abcde-full-first) finished its turn.`, "old-session", session.InputOriginRuntime, ""},
		{"agent prose", `Subagent "duplicate name" (abcde-full-first) finished its turn.`, "old-session", session.InputOriginAgent, ""},
		{"user prose", `Subagent "duplicate name" (abcde-full-first) finished its turn.`, "old-session", session.InputOriginUser, ""},
		{"malformed quote", `Subagent "bad\q" (abcde-full-first) finished its turn.`, "old-session", session.InputOriginRuntime, ""},
		{"malformed verb", `Subagent "name" (abcde-full-first) finished its turn.fake`, "old-session", session.InputOriginRuntime, ""},
		{"missing verb", `Subagent "name" (abcde-full-first)`, "old-session", session.InputOriginRuntime, ""},
		{"body mention", `Runtime update. Full response: Subagent "name" (abcde-full-first) finished its turn.`, "old-session", session.InputOriginRuntime, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := subagentindex.New()
			index.Reset(tree)
			m := newModel(animation.NewRuntime(), 180, 30, &service.SessionState{}, index)
			t.Cleanup(m.StopAnimations)
			input := session.UserMessage(tc.body)
			input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = tc.origin, "steer", tc.sender, "old name"
			input.TurnID = "report:" + first
			m.AddInputMessage(input, 0)
			msg := m.messages[0]
			require.Equal(t, tc.want, msg.InputReference.ID)
			if tc.origin != session.InputOriginUser {
				require.Equal(t, tc.sender, msg.SenderID)
			}
			require.Equal(t, tc.body, input.Message.Content)
			if tc.want == "" {
				require.Equal(t, lifecycle.InputReferenceUnknown, msg.InputReference.Kind)
				return
			}
			require.Equal(t, "abcde", msg.InputReference.DisplayID)
			out := ansi.Strip(m.View())
			require.Contains(t, out, "duplicate name (abcde) has finished their work >")
			require.NotContains(t, out, "ref ")
			require.Equal(t, tc.body, msg.ReceivedBody)
			clickReplyChevron(t, m, ">")
			require.Contains(t, ansi.Strip(m.View()), strings.ReplaceAll(tc.body, "\t", "    "))
			require.Empty(t, msg.Content, "runtime copy field remains unchanged")
			index.Clear()
			m.RefreshInputReferences()
			require.Equal(t, lifecycle.InputReferenceUnknown, msg.InputReference.Kind)
			m.View()
			for y := range m.height {
				for x := range m.contentWidth() {
					_, ok := m.InputReferenceAt(x, y)
					require.False(t, ok)
				}
			}
			index.Reset(tree)
			m.RefreshInputReferences()
			require.Equal(t, tc.want, msg.InputReference.ID)
			require.Equal(t, tc.body, msg.ReceivedBody)
		})
	}
	m := newModel(animation.NewRuntime(), 180, 30, &service.SessionState{})
	msg := types.Input(func() *session.Message {
		input := session.UserMessage(`Subagent "name" (abcde-full-first) finished its turn.`)
		input.InputOrigin, input.InputMode, input.SenderID = session.InputOriginRuntime, "steer", "old-session"
		return input
	}())
	require.Equal(t, lifecycle.InputReferenceUnknown, m.resolveInputReference(msg).Kind, "absent registry cannot invent a link")
}
