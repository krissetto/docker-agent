package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCloneAssistantPresentation(t *testing.T) {
	result := "original result"
	original := chat.Message{Presentation: []chat.AssistantPart{
		{Type: chat.AssistantPartReasoning, Text: "original reasoning"},
		{Type: chat.AssistantPartToolCall, ToolCallID: "call", Tool: &chat.AssistantTool{
			Call:       tools.ToolCall{ID: "call"},
			Definition: tools.Tool{Parameters: map[string]any{"description": "original schema"}, Metadata: map[string]string{"label": "original metadata"}},
			Result:     &result,
		}},
	}}
	cloned := cloneChatMessage(original)
	cloned.Presentation[0].Text = "changed"
	cloned.Presentation[1].Tool.Call.ID = "changed"
	cloned.Presentation[1].Tool.Definition.Parameters.(map[string]any)["description"] = "changed"
	*cloned.Presentation[1].Tool.Result = "changed"
	cloned.Presentation[1].Tool.Definition.Metadata["label"] = "changed"
	require.Equal(t, "original reasoning", original.Presentation[0].Text)
	require.Equal(t, "call", original.Presentation[1].Tool.Call.ID)
	require.Equal(t, "original schema", original.Presentation[1].Tool.Definition.Parameters.(map[string]any)["description"])
	require.Equal(t, "original result", *original.Presentation[1].Tool.Result)
	require.Equal(t, "original metadata", original.Presentation[1].Tool.Definition.Metadata["label"])
	require.Nil(t, cloneChatMessage(chat.Message{}).Presentation)
}

func TestDisplayOnlyAssistantExcludedFromModelInput(t *testing.T) {
	sess := New(WithUserMessage("task"))
	a := agent.New("worker", "test")
	before := sess.GetMessages(a)
	compactionBefore, positionsBefore, _ := sess.CompactionInput()
	msg := &Message{AgentName: "worker", DisplayOnly: true, Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "thought", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "thought"}}}}
	sess.AddMessage(msg)
	require.Equal(t, before, sess.GetMessages(a))
	compactionAfter, positionsAfter, total := sess.CompactionInput()
	require.Equal(t, compactionBefore, compactionAfter)
	require.Equal(t, positionsBefore, positionsAfter)
	require.Equal(t, 2, total)
	require.True(t, sess.Clone().OwnMessages()[1].DisplayOnly)
	encoded, err := json.Marshal(sess)
	require.NoError(t, err)
	var restored Session
	require.NoError(t, json.Unmarshal(encoded, &restored))
	require.True(t, restored.OwnMessages()[1].DisplayOnly)
	require.Equal(t, before, restored.GetMessages(a))
}
