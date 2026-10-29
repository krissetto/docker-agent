package chat

import (
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// AssistantPart records display order without changing provider request fields.
// A nil Message.Presentation denotes legacy history whose order was not recorded.
type AssistantPart struct {
	Type       AssistantPartType `json:"type"`
	Text       string            `json:"text,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Tool       *AssistantTool    `json:"tool,omitempty"`
}

// AssistantTool carries display-only tools from harnesses, which must not enter
// provider request ToolCalls. Model tools use ToolCallID references instead.
type AssistantTool struct {
	Call       tools.ToolCall `json:"call"`
	Definition tools.Tool     `json:"definition"`
	Result     *string        `json:"result,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
}

type AssistantPartType string

const (
	AssistantPartReasoning AssistantPartType = "reasoning"
	AssistantPartContent   AssistantPartType = "content"
	AssistantPartToolCall  AssistantPartType = "tool_call"
)

// AssistantTextAppender is AppendAssistantPart for streamed text without recopying the trailing text.
// Returned strings are immutable; must not be copied after first use.
type AssistantTextAppender struct {
	text strings.Builder
	kind AssistantPartType
	last string
}

func (a *AssistantTextAppender) Append(parts []AssistantPart, part AssistantPart) []AssistantPart {
	if part.Type == AssistantPartToolCall || part.Text == "" {
		return AppendAssistantPart(parts, part)
	}
	n := len(parts)
	if n == 0 || parts[n-1].Type != part.Type {
		a.start(part.Type, part.Text)
		part.Text = a.last
		return append(parts, part)
	}
	last := &parts[n-1]
	if a.kind != part.Type || a.last != last.Text {
		a.start(part.Type, last.Text)
	}
	a.text.WriteString(part.Text)
	a.last = a.text.String()
	last.Text = a.last
	return parts
}

// Reset, never truncate: handed-out strings must keep their bytes.
func (a *AssistantTextAppender) start(kind AssistantPartType, seed string) {
	a.text.Reset()
	a.text.WriteString(seed)
	a.kind = kind
	a.last = a.text.String()
}

// AppendAssistantPart coalesces adjacent text deltas, but never across a tool or
// a change of kind. Tool updates keep the position of their first appearance.
func AppendAssistantPart(parts []AssistantPart, part AssistantPart) []AssistantPart {
	if part.Type == AssistantPartToolCall {
		if part.ToolCallID == "" {
			return parts
		}
		for _, existing := range parts {
			if existing.Type == AssistantPartToolCall && existing.ToolCallID == part.ToolCallID {
				return parts
			}
		}
	} else {
		if part.Text == "" {
			return parts
		}
		if n := len(parts); n > 0 && parts[n-1].Type == part.Type {
			parts[n-1].Text += part.Text
			return parts
		}
	}
	return append(parts, part)
}
