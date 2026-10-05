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
	"github.com/docker/docker-agent/pkg/tui/components/messages"
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
	for _, tc := range []struct {
		name   string
		origin session.InputOrigin
		mode   string
		state  subagent.NodeState
		parent bool
	}{
		{"active explicit reply", session.InputOriginAgent, "steer", subagent.NodeRunning, false},
		{"completed explicit reply", session.InputOriginAgent, "steer", subagent.NodeCompleted, false},
		{"active report", session.InputOriginRuntime, "steer", subagent.NodeRunning, false},
		{"completed report", session.InputOriginRuntime, "steer", subagent.NodeCompleted, false},
		{"nested parent delegation", session.InputOriginAgent, "turn", subagent.NodeRunning, true},
		{"nested parent steering", session.InputOriginAgent, "steer", subagent.NodeRunning, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parentSender := tc.parent
			tree.Nodes[0].Children[0].Node.State = tc.state
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
			input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = tc.origin, tc.mode, childSession, "worker"
			label := "worker (b5678)"
			if parentSender {
				input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = tc.origin, tc.mode, parentSession, "director"
				label = "director (a1234)"
			}
			p.messages.AddInputMessage(input, 0)
			p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
			frame := p.messages.View()
			require.Contains(t, ansi.Strip(frame), label)
			if parentSender {
				assert.NotContains(t, ansi.Strip(frame), "literal body should not navigate")
				assert.Contains(t, ansi.Strip(frame), "> sent a message")
			} else {
				if tc.origin == session.InputOriginRuntime {
					assert.Contains(t, ansi.Strip(frame), "> · report received")
				} else {
					assert.Contains(t, ansi.Strip(frame), "> has replied")
				}
				assert.NotContains(t, ansi.Strip(frame), "literal body should not navigate")
			}
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

type referenceRefreshCounter struct {
	messages.Model
	refreshes int
}

func (m *referenceRefreshCounter) RefreshInputReferences() {
	m.refreshes++
	m.Model.RefreshInputReferences()
}

func TestTreeMetricsDoNotRefreshTranscriptReferences(t *testing.T) {
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	counter := &referenceRefreshCounter{Model: p.messages}
	p.messages = counter
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", SessionID: "child-session", Agent: "worker"}}}}
	handled, _ := p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
	require.True(t, handled)
	require.Equal(t, 1, counter.refreshes)
	for j := range 10 {
		tree.Nodes[0].Node.Cost = float64(j)
		tree.Nodes[0].Node.OutputTokens = int64(j)
		tree.Nodes[0].Node.State = subagent.NodeRunning
		tree.Nodes[0].Node.WaitingOn = "tool"
		p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
	}
	assert.Equal(t, 1, counter.refreshes, "metric/activity trees must not walk or invalidate the transcript")
	tree.Nodes[0].Node.Name = "Renamed worker"
	p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
	assert.Equal(t, 2, counter.refreshes)
	assert.Equal(t, "Renamed worker", p.subagents.Resolve("", "child-session", "").Name)
	p.handleRuntimeEvent(&runtime.SubagentTreeEvent{})
	assert.Equal(t, 3, counter.refreshes, "authoritative empty tree removes stale references")
}
