package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestMessageCardIdentitySingleAttachAndStableHeader(t *testing.T) {
	const parentSession = "parent-session-full"
	const parentNode = "abcde-parent-full"
	const targetNode = "abcde-target-full"
	for _, target := range []string{"parent", targetNode} {
		for _, nested := range []bool{false, true} {
			for _, expanded := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/nested=%t/expanded=%t", target, nested, expanded), func(t *testing.T) {
					tree := subagent.Snapshot{Root: parentNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: parentNode, SessionID: parentSession, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "sender-node", SessionID: "sender-session", Parent: parentNode, Agent: "worker"}}, {Node: subagent.Node{ID: targetNode, SessionID: "target-session", Parent: parentNode, Name: "Named engineer", Agent: "engineer"}}}}}}
					sess := session.New(session.WithID("sender-session"), session.WithParentID(parentSession))
					sess.SetSubagentTree(&tree)
					a, _ := newSessionTestApp(t, sess, nil, nil)
					app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "sender-node", Session: sess, ParentSessionID: parentSession, ParentAgent: "root"})(a)
					ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
					p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
					t.Cleanup(func() { Cleanup(p); ar.Stop() })
					p.sessionState.SetExpandThinking(false)
					p.SetRoutingID("tab-routing-id")
					p.SetSize(160, 40)
					p.resetProjection(runtime.SessionSnapshot{Session: sess})
					if nested {
						p.messages.AppendReasoning("worker", "Thinking")
					}
					call := tools.ToolCall{ID: "send", Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: fmt.Sprintf(`{"to":%q,"message":"literal request"}`, target)}}
					p.messages.AddOrUpdateToolCall("worker", call, tools.Tool{}, types.ToolStatusCompleted)
					xbase := styles.AppPadding + p.computeSidebarLayout().chatStartX
					if nested {
						frame := p.messages.View()
						for y, line := range strings.Split(frame, "\n") {
							if strings.Contains(ansi.Strip(line), "Thinking") {
								p.messages.Update(tea.MouseClickMsg{X: xbase, Y: y, Button: tea.MouseLeft})
								break
							}
						}
					}
					findHeader := func() (int, int, string) {
						t.Helper()
						label := "Named engineer (abcde)"
						if target == "parent" {
							label = "root (abcde)"
						}
						for y, line := range strings.Split(p.messages.View(), "\n") {
							before, _, found := strings.Cut(ansi.Strip(line), label)
							if found {
								return xbase + ansi.StringWidth(before), y, strings.TrimRight(ansi.Strip(line), " ")
							}
						}
						t.Fatalf("message header missing: %q", ansi.Strip(p.messages.View()))
						return 0, 0, ""
					}
					settle := func() {
						t.Helper()
						for ar.HasActive() {
							tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
							require.True(t, ok)
							p.Update(tick)
						}
					}
					x, y, collapsed := findHeader()
					ref, ok := p.messages.InputReferenceAt(x, y)
					require.True(t, ok)
					expectedRefID := targetNode
					expectedKind := lifecycle.InputReferenceNode
					if target == "parent" {
						expectedRefID, expectedKind = parentSession, lifecycle.InputReferenceParent
					}
					require.Equal(t, expectedRefID, ref.ID)
					require.Equal(t, expectedKind, ref.Kind)
					require.NotContains(t, collapsed, "Messaged parent")
					if expanded {
						// Even trailing header whitespace is a disclosure hit, not attachment.
						_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x + len(ref.Label()) + 8, Y: y, Button: tea.MouseLeft})
						for _, event := range runTimerCmd(t, cmd) {
							switch event.(type) {
							case msgtypes.OpenSubagentMsg, msgtypes.SwitchTabMsg:
								t.Fatal("row attached on single click")
							}
						}
						settle()
					}
					x0, y0, before := findHeader()
					p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
					require.True(t, ar.HasActive(), "parent and named identity share hover animation")
					settle()
					click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
					_, cmd := p.handleMouseClick(click)
					events := runTimerCmd(t, cmd)
					if target == "parent" {
						require.Contains(t, events, msgtypes.SwitchTabMsg{SessionID: parentSession})
					} else {
						require.Contains(t, events, msgtypes.OpenSubagentMsg{NodeID: targetNode})
					}
					require.Contains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "tab-routing-id"})
					_, _, attached := findHeader()
					require.Equal(t, before, attached, "single identity click attaches without toggling")
					settle()
					x1, y1, after := findHeader()
					require.Equal(t, x0, x1)
					require.Equal(t, y0, y1)
					require.Equal(t, before, after)
					refAfter, ok := p.messages.InputReferenceAt(x, y)
					require.True(t, ok)
					require.Equal(t, ref, refAfter)
					require.Equal(t, expanded, strings.Contains(ansi.Strip(p.messages.View()), "literal request"))
					require.NotContains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "tab-routing-id", Text: "Double-click to attach"})
					// The visible neutral ID shares navigation, never disclosure.
					_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: x + ansi.StringWidth(ref.Name) + 2, Y: y, Button: tea.MouseLeft})
					idEvents := runTimerCmd(t, cmd)
					if target == "parent" {
						require.Contains(t, idEvents, msgtypes.SwitchTabMsg{SessionID: parentSession})
					} else {
						require.Contains(t, idEvents, msgtypes.OpenSubagentMsg{NodeID: targetNode})
					}
					_, _, afterID := findHeader()
					require.Equal(t, before, afterID)
				})
			}
		}
	}
}

func TestMessageCardIdentityAttachesAfterOtherGestures(t *testing.T) {
	for _, interruption := range []string{"key", "wheel", "outside", "modal"} {
		t.Run(interruption, func(t *testing.T) {
			sess := session.New()
			tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full", SessionID: "child-session", Agent: "worker"}}}}
			sess.SetSubagentTree(&tree)
			a, _ := newSessionTestApp(t, sess, nil, nil)
			ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
			p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			t.Cleanup(func() { Cleanup(p); ar.Stop() })
			p.SetSize(160, 40)
			p.resetProjection(runtime.SessionSnapshot{Session: sess})
			p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
			p.messages.AddOrUpdateToolCall("root", tools.ToolCall{ID: "send", Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"child-full","message":"payload"}`}}, tools.Tool{}, types.ToolStatusCompleted)
			x, y := -1, -1
			for row, line := range strings.Split(p.messages.View(), "\n") {
				if before, _, found := strings.Cut(ansi.Strip(line), "worker (child)"); found {
					x, y = styles.AppPadding+p.computeSidebarLayout().chatStartX+ansi.StringWidth(before), row
					break
				}
			}
			require.GreaterOrEqual(t, x, 0)
			click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
			switch interruption {
			case "key":
				p.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "wheel":
				p.Update(msgtypes.WheelCoalescedMsg{X: x, Y: y, Delta: 1})
			case "outside":
				p.handleMouseClick(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseRight})
			case "modal":
				ClearSidebarHover(p)
			}
			_, cmd := p.handleMouseClick(click)
			require.Contains(t, runTimerCmd(t, cmd), msgtypes.OpenSubagentMsg{NodeID: "child-full"})
			settleSidebarGesture(t, p)
			require.NotContains(t, ansi.Strip(p.messages.View()), "payload", "identity never toggles")
		})
	}
}
