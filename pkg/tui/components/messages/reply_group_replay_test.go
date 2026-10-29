package messages

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestVisibleRepliesLiveAndReplayKeepLogicalInterleavedAnswers(t *testing.T) {
	for _, policy := range []msgtypes.VisibleReplies{msgtypes.VisibleRepliesLatest, msgtypes.VisibleRepliesThree} {
		t.Run(string(policy), func(t *testing.T) {
			newModel := func() *model {
				ar := animation.NewRuntime()
				m := NewScrollableView(ar, 80, 40, &service.SessionState{}).(*model)
				t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
				m.SetCompactWorkLog(true)
				m.SetVisibleReplies(policy)
				m.suppressReplyHandoff = true
				return m
			}
			live, replay := newModel(), newModel()
			sess := session.New()
			sess.AddMessage(session.UserMessage("request"))
			live.AddUserMessage("request")
			for i := range 4 {
				first, last := fmt.Sprintf("answer %d first", i), fmt.Sprintf("answer %d last", i)
				call := tools.ToolCall{ID: fmt.Sprintf("call-%d", i), Function: tools.FunctionCall{Name: "check"}}
				live.AppendToLastMessage("root", first)
				live.AppendReasoning("root", "thinking")
				live.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
				live.AppendToLastMessage("root", last)
				stored := session.NewAgentMessage("root", &chat.Message{
					Role: chat.MessageRoleAssistant,
					Presentation: []chat.AssistantPart{
						{Type: chat.AssistantPartContent, Text: first},
						{Type: chat.AssistantPartReasoning, Text: "thinking"},
						{Type: chat.AssistantPartToolCall, ToolCallID: call.ID},
						{Type: chat.AssistantPartContent, Text: last},
					},
					ToolCalls: []tools.ToolCall{call},
				})
				sess.AddMessage(stored)
				live.CompleteAssistant(&runtime.MessageAddedEvent{Message: stored})
				live.syncWorkProjection()
				require.Len(t, live.workProjection.exchanges[0].response, 2*min(i+1, visibleReplyLimit(policy)), "interleaved parts consume only one reply slot even before replay")
			}
			replay.LoadFromSession(sess, nil)
			live.syncWorkProjection()
			replay.syncWorkProjection()
			texts := func(m *model) []string {
				var result []string
				for _, source := range m.workProjection.exchanges[0].response {
					result = append(result, m.messages[source].Content)
				}
				return result
			}
			require.Equal(t, texts(live), texts(replay), "reload preserves logical reply retention")
		})
	}
}
