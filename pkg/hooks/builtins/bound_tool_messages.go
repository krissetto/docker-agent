package builtins

import (
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// BoundToolMessages repairs model-facing copies of historical results using
// the definitions saved with their calls. Unknown tools and user text are left
// untouched; neither the message slice nor its nested parts are mutated.
func BoundToolMessages(messages []chat.Message) []chat.Message {
	calls := make(map[string]tools.Tool)
	var bounded []chat.Message
	for i, msg := range messages {
		if msg.Role == chat.MessageRoleAssistant {
			for _, call := range msg.ToolCalls {
				if call.ID == "" {
					continue
				}
				delete(calls, call.ID)
				for _, definition := range msg.ToolDefinitions {
					if definition.Name == call.Function.Name {
						calls[call.ID] = definition
						break
					}
				}
			}
		}
		if msg.Role != chat.MessageRoleTool {
			continue
		}
		tool, ok := calls[msg.ToolCallID]
		if !ok || !largeResultCategories[tool.Category] {
			continue
		}
		const notice = "Omitted output remains in the original session history; narrow the tool query to retrieve the part you need."
		remaining := maxToolCallResultBytes
		fit := func(text string) string {
			kept := boundHistoricalToolText(tool, text, notice, remaining)
			remaining -= len(kept)
			return kept
		}
		content := fit(msg.Content)
		var parts []chat.MessagePart
		contentDuplicateSynced := false
		for j, part := range msg.MultiContent {
			updated := part
			switch {
			case part.Type == chat.MessagePartTypeText && !contentDuplicateSynced && part.Text == msg.Content:
				updated.Text = content
				contentDuplicateSynced = true
			case part.Type == chat.MessagePartTypeText:
				updated.Text = fit(part.Text)
			case part.Type == chat.MessagePartTypeDocument && part.Document != nil:
				text := fit(part.Document.Source.InlineText)
				if text == part.Document.Source.InlineText {
					continue
				}
				doc := *part.Document
				doc.Source.InlineText = text
				doc.Size = int64(len(text))
				updated.Document = &doc
			}
			if updated.Text == part.Text && updated.Document == part.Document {
				continue
			}
			if parts == nil {
				parts = slices.Clone(msg.MultiContent)
			}
			parts[j] = updated
		}
		if content == msg.Content && parts == nil {
			continue
		}
		if bounded == nil {
			bounded = slices.Clone(messages)
		}
		bounded[i].Content = content
		if parts != nil {
			bounded[i].MultiContent = parts
		}
	}
	if bounded == nil {
		return messages
	}
	return bounded
}

// Like the configurable session cap, the independent backstop charges one
// result-wide budget across text and inline documents, counting Content's first
// duplicate once. This projection never changes saved definitions or media.
func boundHistoricalToolText(tool tools.Tool, text, notice string, budget int) string {
	if len(text) <= budget {
		return text
	}
	if budget == maxToolCallResultBytes {
		return BoundToolResult(tool.Category, tool.Name, text, notice)
	}
	if budget == 0 {
		return ""
	}
	prefix := chat.TruncateUTF8Bytes("Tool output omitted. "+notice+"\n\n", budget/2)
	available := budget - len(prefix)
	if tool.Category == filesystemToolCategory && tool.Name == readFileToolName {
		return prefix + chat.TruncateUTF8Bytes(text, available)
	}
	return prefix + string(trimToRuneStart([]byte(text[len(text)-available:])))
}
