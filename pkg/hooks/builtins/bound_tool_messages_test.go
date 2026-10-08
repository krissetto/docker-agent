package builtins_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestBoundToolMessagesPreservesHistoryAndPairing(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("世", 50*1024)
	media := &chat.MessageImageURL{URL: "data:image/png;base64,aGVsbG8="}
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: payload},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "logs"}}}, ToolDefinitions: []tools.Tool{{Name: "logs", Category: "background_jobs"}}},
		{Role: chat.MessageRoleTool, ToolCallID: "call", IsError: true, Content: payload, MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: payload}, {Type: chat.MessagePartTypeImageURL, ImageURL: media}}},
		{Role: chat.MessageRoleTool, ToolCallID: "unknown", Content: payload},
	}
	got := builtins.BoundToolMessages(msgs)
	require.Len(t, got, len(msgs))
	assert.Equal(t, msgs[0], got[0])
	assert.Equal(t, msgs[1], got[1])
	assert.Equal(t, msgs[3], got[3])
	assert.Equal(t, "call", got[2].ToolCallID)
	assert.True(t, got[2].IsError)
	assert.LessOrEqual(t, len(got[2].Content), 50*1024)
	assert.Equal(t, got[2].Content, got[2].MultiContent[0].Text)
	assert.Equal(t, media, got[2].MultiContent[1].ImageURL)
	assert.Equal(t, payload, msgs[2].Content)
	assert.Equal(t, payload, msgs[2].MultiContent[0].Text)
	assert.Equal(t, got, builtins.BoundToolMessages(got))
}

func TestBoundToolMessagesRequiresMatchingSavedDefinition(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"", "memory", "custom"} {
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			msgs := []chat.Message{
				{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "shell"}}}, ToolDefinitions: []tools.Tool{{Name: "shell", Category: category}}},
				{Role: chat.MessageRoleTool, ToolCallID: "call", Content: strings.Repeat("x", 100_000)},
			}
			assert.Equal(t, msgs, builtins.BoundToolMessages(msgs))
		})
	}
}

func TestBoundToolMessagesReadFileHeadAndEmptyID(t *testing.T) {
	t.Parallel()
	payload := "important beginning\n" + strings.Repeat("x", 100_000) + "discarded tail"
	for _, id := range []string{"call", ""} {
		msgs := []chat.Message{
			{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: id, Function: tools.FunctionCall{Name: "read_file"}}}, ToolDefinitions: []tools.Tool{{Name: "read_file", Category: "filesystem"}}},
			{Role: chat.MessageRoleTool, ToolCallID: id, Content: payload},
		}
		got := builtins.BoundToolMessages(msgs)
		if id == "" {
			assert.Equal(t, msgs, got)
			continue
		}
		assert.Contains(t, got[1].Content, "important beginning")
		assert.NotContains(t, got[1].Content, "discarded tail")
		assert.LessOrEqual(t, len(got[1].Content), 50*1024)
	}
}

func TestBoundToolMessagesAggregateTextAndDocuments(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"", strings.Repeat("世", 10_000), strings.Repeat("世", 30_000)} {
		payload := strings.Repeat("界", 30_000)
		doc := &chat.Document{Name: "log.txt", MimeType: "text/plain", Size: int64(len(payload)), Source: chat.DocumentSource{InlineText: payload}}
		media := &chat.Document{Name: "image.png", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1, 2, 3}}}
		msgs := []chat.Message{
			{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "shell"}}}, ToolDefinitions: []tools.Tool{{Name: "shell", Category: "shell"}}},
			{Role: chat.MessageRoleTool, ToolCallID: "call", Content: content, MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: content},
				{Type: chat.MessagePartTypeText, Text: content},
				{Type: chat.MessagePartTypeDocument, Document: doc},
				{Type: chat.MessagePartTypeDocument, Document: doc},
				{Type: chat.MessagePartTypeDocument, Document: media},
			}},
		}
		got := builtins.BoundToolMessages(msgs)
		result := got[1]
		bytes := len(result.Content) + len(result.MultiContent[1].Text)
		assert.Equal(t, result.Content, result.MultiContent[0].Text)
		for _, part := range result.MultiContent[2:4] {
			bytes += len(part.Document.Source.InlineText)
			assert.Equal(t, part.Document.Name, doc.Name)
			assert.Equal(t, part.Document.MimeType, doc.MimeType)
			assert.Equal(t, int64(len(part.Document.Source.InlineText)), part.Document.Size)
			assert.Equal(t, part.Document.Source.InlineText, strings.ToValidUTF8(part.Document.Source.InlineText, ""))
		}
		assert.LessOrEqual(t, bytes, 50*1024)
		assert.Same(t, media, result.MultiContent[4].Document)
		assert.Equal(t, content, msgs[1].Content)
		assert.Equal(t, content, msgs[1].MultiContent[1].Text)
		assert.Equal(t, payload, doc.Source.InlineText)
		assert.Equal(t, int64(len(payload)), doc.Size)
		assert.Equal(t, got, builtins.BoundToolMessages(got))
	}
}

func TestBoundToolMessagesRejectsStaleAndDisplayOnlyDefinitions(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("x", 100_000)
	call := tools.ToolCall{ID: "reuse", Function: tools.FunctionCall{Name: "logs"}}
	msgs := []chat.Message{
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "logs", Category: "shell"}}},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}},
		{Role: chat.MessageRoleTool, ToolCallID: "reuse", Content: payload},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "mismatch", Function: tools.FunctionCall{Name: "other"}}}, ToolDefinitions: []tools.Tool{{Name: "logs", Category: "shell"}}},
		{Role: chat.MessageRoleTool, ToolCallID: "mismatch", Content: payload},
		{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, Tool: &chat.AssistantTool{Call: tools.ToolCall{ID: "display", Function: tools.FunctionCall{Name: "logs"}}, Definition: tools.Tool{Name: "logs", Category: "shell"}, Result: &payload}}}},
		{Role: chat.MessageRoleTool, ToolCallID: "display", Content: payload},
	}
	assert.Equal(t, msgs, builtins.BoundToolMessages(msgs))
}
