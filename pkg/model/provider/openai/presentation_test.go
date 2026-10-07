package openai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestPresentationDoesNotChangeProviderRequests(t *testing.T) {
	messages := []chat.Message{{Role: chat.MessageRoleAssistant, Content: "answer", ReasoningContent: "reasoning"}}
	chatBefore, err := json.Marshal(oaistream.ConvertMessagesWithCaps(t.Context(), messages, modelinfo.ModelCapabilities{}))
	require.NoError(t, err)
	responsesBefore, err := json.Marshal((&Client{}).convertMessagesToResponseInput(t.Context(), messages))
	require.NoError(t, err)
	result := "display-only result"
	messages[0].Presentation = []chat.AssistantPart{
		{Type: chat.AssistantPartContent, Text: "answer"},
		{Type: chat.AssistantPartToolCall, ToolCallID: "display-only", Tool: &chat.AssistantTool{
			Call:       tools.ToolCall{ID: "display-only", Function: tools.FunctionCall{Name: "external", Arguments: "{}"}},
			Definition: tools.Tool{Name: "external"}, Result: &result,
		}},
		{Type: chat.AssistantPartReasoning, Text: "reasoning"},
	}
	chatAfter, err := json.Marshal(oaistream.ConvertMessagesWithCaps(t.Context(), messages, modelinfo.ModelCapabilities{}))
	require.NoError(t, err)
	responsesAfter, err := json.Marshal((&Client{}).convertMessagesToResponseInput(t.Context(), messages))
	require.NoError(t, err)
	require.JSONEq(t, string(chatBefore), string(chatAfter))
	require.JSONEq(t, string(responsesBefore), string(responsesAfter))
}

func TestResponseStreamPreservesPresentationEventOrder(t *testing.T) {
	stream := &fakeEventStream{events: decodeEvents(t, []map[string]any{
		{"type": "response.reasoning_summary_text.delta", "item_id": "reason", "delta": "first"},
		{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item", "call_id": "call", "name": "check"}},
		{"type": "response.output_text.delta", "item_id": "message", "delta": "commentary"},
		{"type": "response.function_call_arguments.delta", "item_id": "item", "delta": "{}"},
		{"type": "response.reasoning_summary_text.delta", "item_id": "reason", "delta": "second"},
		toolCallsCompletedEvent("response"),
	})}
	adapter := newResponseStreamAdapter(stream, true)
	defer adapter.Close()
	var parts []chat.AssistantPart
	var arguments string
	for range len(stream.events) {
		resp, err := adapter.Recv()
		require.NoError(t, err)
		for _, choice := range resp.Choices {
			delta := choice.Delta
			parts = chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: delta.ReasoningContent})
			parts = chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartContent, Text: delta.Content})
			for _, call := range delta.ToolCalls {
				parts = chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartToolCall, ToolCallID: call.ID})
				arguments += call.Function.Arguments
			}
		}
	}
	require.Equal(t, []chat.AssistantPart{
		{Type: chat.AssistantPartReasoning, Text: "first"},
		{Type: chat.AssistantPartToolCall, ToolCallID: "call"},
		{Type: chat.AssistantPartContent, Text: "commentary"},
		{Type: chat.AssistantPartReasoning, Text: "second"},
	}, parts)
	require.Equal(t, "{}", arguments)
}
