package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestPendingResponseHandsOffOnlyOnActualOutput(t *testing.T) {
	for _, first := range []string{"content", "reasoning", "tool"} {
		t.Run(first, func(t *testing.T) {
			sess := session.New()
			ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
			t.Cleanup(ar.Stop)
			a, _ := newSessionTestApp(t, sess, nil, nil)
			p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			t.Cleanup(func() { Cleanup(p) })
			p.messages.SetSize(100, 40)
			p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: sess.ID, Message: "current request", SessionPosition: -1})
			p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
			check := func(pending bool, working bool) string {
				t.Helper()
				for range 3 {
					if cmd := ar.Continue(); cmd != nil {
						if tick, ok := ar.Accept(cmd().(animation.TickMsg)); ok {
							p.Update(tick)
						}
					}
					frame := ansi.Strip(p.messages.View())
					if pending {
						require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeSpinner), frame)
					} else {
						require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner), frame)
					}
					require.Equal(t, working, strings.Contains(frame, "Working [+]") || strings.Contains(frame, "Working [-]"), frame)
					require.Contains(t, frame, "current request")
				}
				return ansi.Strip(p.messages.View())
			}
			check(true, false)
			p.handleRuntimeEvent(runtime.StreamStarted("waiting-child", "worker"))
			check(true, false)
			p.handleRuntimeEvent(runtime.StreamStopped("waiting-child", "worker", "normal"))
			check(true, false)
			for _, envelope := range []runtime.Event{
				runtime.AgentChoice("root", sess.ID, ""),
				runtime.AgentChoiceReasoning("root", sess.ID, ""),
				&runtime.PartialToolCallEvent{},
				&runtime.AgentInfoEvent{AgentName: "root", Model: "fixture"},
			} {
				p.handleRuntimeEvent(envelope)
				check(true, false)
			}
			switch first {
			case "content":
				p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "FIRST_CONTENT"))
			case "reasoning":
				p.handleRuntimeEvent(runtime.AgentChoiceReasoning("root", sess.ID, "FIRST_REASONING"))
			case "tool":
				p.handleRuntimeEvent(runtime.PartialToolCall(tools.ToolCall{ID: "first-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}, tools.Tool{Name: "shell"}, "root"))
			}
			frame := check(false, first != "content")
			if first == "content" {
				require.Contains(t, frame, "FIRST_CONTENT")
			}
			p.handleRuntimeEvent(runtime.StreamStarted("child", "worker"))
			check(false, first != "content")
			p.handleRuntimeEvent(runtime.StreamStopped("child", "worker", "normal"))
			check(false, first != "content")
			if first == "content" {
				p.handleRuntimeEvent(runtime.AgentChoiceReasoning("root", sess.ID, "LATER_REASONING"))
				for range 20 {
					if cmd := ar.Continue(); cmd != nil {
						if tick, ok := ar.Accept(cmd().(animation.TickMsg)); ok {
							p.Update(tick)
						}
					}
					require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner), p.messages.View())
				}
				check(false, true)
			}
			p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
			check(false, false)
			p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: sess.ID, Message: "next request", SessionPosition: -1})
			p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
			require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeSpinner), "previous turn must not suppress next pending")
			require.Contains(t, ansi.Strip(p.messages.View()), "current request", "history remains intact")
		})
	}
}
