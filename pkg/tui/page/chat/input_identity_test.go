package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestInputIdentityChatClickUsesCanonicalChildAndNestedParent(t *testing.T) {
	const parentSession = "parent-session-full-uuid"
	const childSession = "child-session-full-uuid"
	const parentNode = "a1234"
	const childNode = "b5678"
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: parentNode, SessionID: parentSession, Agent: "director"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: childNode, SessionID: childSession, Parent: parentNode, Agent: "worker"}}}}}}
	for _, parentSender := range []bool{false, true} {
		t.Run(map[bool]string{false: "child report", true: "nested parent delegation"}[parentSender], func(t *testing.T) {
			sess := session.New(session.WithID("view-session"))
			if parentSender {
				sess.ParentID = parentSession
			}
			sess.SetSubagentTree(&tree)
			a, _ := newSessionTestApp(t, sess, nil, nil)
			if parentSender {
				app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: childNode, Session: sess, ParentSessionID: parentSession, ParentAgent: "director"})(a)
			}
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			p.SetSize(160, 40)
			p.resetProjection(runtime.SessionSnapshot{Session: sess})
			input := session.UserMessage("literal body should not navigate")
			input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = session.InputOriginRuntime, "steer", childSession, "worker"
			label := "worker (b5678)"
			if parentSender {
				input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = session.InputOriginAgent, "turn", parentSession, "director"
				label = "director (a1234)"
			}
			p.messages.AddInputMessage(input, 0)
			p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
			frame := p.messages.View()
			require.Contains(t, ansi.Strip(frame), label)
			sl := p.computeSidebarLayout()
			for y, line := range strings.Split(frame, "\n") {
				before, _, ok := strings.Cut(ansi.Strip(line), label)
				if !ok {
					continue
				}
				x := styles.AppPadding + sl.chatStartX + ansi.StringWidth(before)
				_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.NotNil(t, cmd)
				if parentSender {
					assert.Contains(t, runTimerCmd(t, cmd), msgtypes.SwitchTabMsg{SessionID: parentSession})
				} else {
					assert.Contains(t, runTimerCmd(t, cmd), msgtypes.OpenSubagentMsg{NodeID: childNode})
				}
				return
			}
			t.Fatal("rendered sender coordinate missing")
		})
	}
}
