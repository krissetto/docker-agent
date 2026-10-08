package compactor

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCompactorBoundsHistoricalResultWithoutChangingHistory(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("世", 1_000_000) + "diagnostic tail"
	sess := session.New(session.WithUserMessage("inspect the job"))
	presentation := []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: "old-call"}}
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "old-call", Function: tools.FunctionCall{Name: "logs"}}},
		ToolDefinitions: []tools.Tool{{Name: "logs", Category: "background_jobs"}}, Presentation: presentation,
	}})
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role: chat.MessageRoleTool, ToolCallID: "old-call", Content: payload, IsError: true,
		MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: payload}},
	}})
	sess.AddMessage(&session.Message{DisplayOnly: true, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "display-only"}})
	sess.AddMessage(session.UserMessage("continue"))
	// The bounded result fits the kept-tail budget on a large window.
	assert.Equal(t, sess.ItemCount(), ComputeFirstKeptEntry(sess, 100_000))
	// On the smaller window it is summarized, not dropped as an oversized item.
	assert.Equal(t, 4, ComputeFirstKeptEntry(sess, 32_000))
	a := agent.New("root", "instructions", agent.WithModel(fakeProvider{id: modelsdev.NewID("fake", "model")}))
	result, err := RunLLM(t.Context(), LLMArgs{
		Session: sess, Agent: a, ContextLimit: 32_000,
		RunAgent: func(_ context.Context, _ *agent.Agent, cs *session.Session) error {
			msgs, _, _ := cs.CompactionInput()
			found := false
			for _, msg := range msgs {
				assert.NotEqual(t, "display-only", msg.Content)
				if msg.Role == chat.MessageRoleAssistant {
					assert.Equal(t, presentation, msg.Presentation)
				}
				if msg.Role == chat.MessageRoleTool {
					found = true
					assert.Equal(t, "old-call", msg.ToolCallID)
					assert.True(t, msg.IsError)
					assert.LessOrEqual(t, len(msg.Content), 50*1024)
					assert.Equal(t, msg.Content, msg.MultiContent[0].Text)
					assert.Contains(t, msg.Content, "diagnostic tail")
				}
			}
			require.True(t, found, "known oversized result must reach the summarizer")
			cs.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "summary"}})
			return nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "summary", result.Summary)
	assert.Equal(t, 4, result.FirstKeptEntry)
	assert.Equal(t, payload, sess.Messages[2].Message.Message.Content)
	assert.Equal(t, payload, sess.Messages[2].Message.Message.MultiContent[0].Text)
	assert.Equal(t, presentation, sess.Messages[1].Message.Message.Presentation)
}
