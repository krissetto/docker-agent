package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestInputIdentityLeanLiveCachedTreeAndRestoredParity(t *testing.T) {
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "a1b2c", SessionID: "child-session-uuid", Agent: "worker"}}}}
	m := bareModel(30)
	sess := session.New(session.WithID("parent"))
	for i, mode := range []string{"turn", "steer"} {
		input := session.UserMessage("literal **body**")
		input.InputOrigin, input.InputMode, input.SenderName, input.SenderID, input.TurnID = session.InputOriginAgent, mode, "worker", "child-session-uuid", mode
		sess.AddMessage(input)
		m.handleEvent(t.Context(), &runtime.UserMessageEvent{TurnID: mode, Message: input.Message.Content, InputOrigin: input.InputOrigin, InputMode: mode, SenderName: input.SenderName, SenderID: input.SenderID, SessionPosition: i})
	}
	_ = m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil)
	m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: tree})
	check := func() string {
		out := ansi.Strip(strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n"))
		require.Equal(t, 2, strings.Count(out, "worker (a1b2c)"), "cached blocks refresh canonical identity after tree arrival")
		assert.NotContains(t, out, "Message from")
		assert.NotContains(t, out, "child-session")
		for line := range strings.SplitSeq(out, "\n") {
			if strings.Contains(line, "worker (a1b2c)") {
				assert.Contains(t, line, "━", "sender embedded in border, not header")
			}
		}
		return out
	}
	live := check()
	sess.SetSubagentTree(&tree)
	m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: sess}})
	assert.Equal(t, live, check())
}
