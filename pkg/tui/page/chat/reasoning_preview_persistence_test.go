package chat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatapi "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRuntimeReasoningPreviewPersistsAcrossCommittedToolRounds(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			p, sess := newContentTestPage(t)
			p.SetCompactWorkLog(compact)
			p.messages.AddUserMessage("request")
			p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
			for round := range 2 {
				id := fmt.Sprintf("message-%d", round)
				call := tools.ToolCall{ID: fmt.Sprintf("call-%d", round), Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
				for _, chunk := range []string{"thought ", fmt.Sprint(round)} {
					p.handleRuntimeEvent(runtime.AgentChoiceReasoning("root", sess.ID, chunk, id))
				}
				committed := session.NewAgentMessage("root", &chatapi.Message{
					Role: chatapi.MessageRoleAssistant, MessageID: id, ToolCalls: []tools.ToolCall{call},
					Presentation: []chatapi.AssistantPart{{Type: chatapi.AssistantPartReasoning, Text: fmt.Sprintf("thought %d", round)}, {Type: chatapi.AssistantPartToolCall, ToolCallID: call.ID}},
				})
				for _, event := range []runtime.Event{
					runtime.ToolCall(call, tools.Tool{Name: "check"}, "root"),
					runtime.MessageAdded(sess.ID, committed, "root"),
					runtime.ToolCallResponse(call.ID, tools.Tool{Name: "check"}, &tools.ToolCallResult{Output: "done"}, "done", "root"),
				} {
					p.handleRuntimeEvent(event)
				}
				p.messages.SetSize(99-round, 40)
				frame := ansi.Strip(p.messages.View())
				require.Equal(t, 1, strings.Count(frame, "thought 0"))
				if round > 0 {
					require.Equal(t, 1, strings.Count(frame, "thought 1"))
				}
			}
			p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "answer", "final"))
			p.messages.SetSize(96, 40)
			frame := ansi.Strip(p.messages.View())
			require.Contains(t, frame, "answer")
			if compact {
				require.NotContains(t, frame, "thought 0")
				require.NotContains(t, frame, "thought 1")
			}
		})
	}
}
