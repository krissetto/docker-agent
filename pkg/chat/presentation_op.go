package chat

import (
	"slices"

	"github.com/docker/docker-agent/pkg/tools"
)

// AssistantPartOpKind identifies one tool-presentation update of a streamed
// assistant message. Text deltas are not ops: they go through AppendAssistantPart
// (or AssistantTextAppender) with AssistantPartContent/AssistantPartReasoning.
type AssistantPartOpKind string

const (
	AssistantPartOpToolDelta  AssistantPartOpKind = "tool_delta"
	AssistantPartOpToolCall   AssistantPartOpKind = "tool_call"
	AssistantPartOpToolResult AssistantPartOpKind = "tool_result"
)

// AssistantPartOp is the durable, delta-sized form of one tool presentation
// update. Replaying ops in order through ApplyAssistantPartOp reproduces the
// tool parts that the live runtime builds from partial, call and response events.
type AssistantPartOp struct {
	Kind AssistantPartOpKind `json:"k"`
	// ToolCallID identifies the tool part. For tool_call the ID is Call.ID.
	ToolCallID string `json:"id,omitempty"`
	// Type is the call type of a tool_delta; it is applied only when non-empty.
	Type string `json:"t,omitempty"`
	// Name is the function name of a tool_delta; it is applied only when non-empty.
	Name string `json:"n,omitempty"`
	// Arguments is the argument DELTA of a tool_delta, appended to the call.
	Arguments string `json:"a,omitempty"`
	// Call is the full call of a tool_call. Its empty arguments keep the previous ones.
	Call *tools.ToolCall `json:"c,omitempty"`
	// Definition replaces the tool definition when non-nil. Callers that omit an
	// unchanged definition must still include it for the first op of a call ID.
	// Callers must pass a value they own; it is copied shallowly.
	Definition *tools.Tool `json:"d,omitempty"`
	// Result is the output of a tool_result. Results never create a tool part.
	Result  *string `json:"r,omitempty"`
	IsError bool    `json:"e,omitempty"`
}

// ApplyAssistantPartOp applies one tool op to parts, mutating them in place as the
// runtime's tool presentation branch does. Unknown kinds leave parts unchanged.
func ApplyAssistantPartOp(parts []AssistantPart, op AssistantPartOp) []AssistantPart {
	var call tools.ToolCall
	var definition *tools.Tool
	var result *string
	var isError, delta bool
	switch op.Kind {
	case AssistantPartOpToolDelta:
		call = tools.ToolCall{
			ID:       op.ToolCallID,
			Type:     tools.ToolType(op.Type),
			Function: tools.FunctionCall{Name: op.Name, Arguments: op.Arguments},
		}
		definition, delta = op.Definition, true
	case AssistantPartOpToolCall:
		if op.Call != nil {
			call = *op.Call
		}
		definition = op.Definition
	case AssistantPartOpToolResult:
		call.ID = op.ToolCallID
		output := ""
		if op.Result != nil {
			output = *op.Result
		}
		result, isError = &output, op.IsError
	default:
		return parts
	}
	idx := slices.IndexFunc(parts, func(part AssistantPart) bool {
		return part.Type == AssistantPartToolCall && part.ToolCallID == call.ID
	})
	if idx < 0 {
		if call.ID == "" || result != nil {
			return parts
		}
		parts = AppendAssistantPart(parts, AssistantPart{Type: AssistantPartToolCall, ToolCallID: call.ID, Tool: &AssistantTool{}})
		idx = len(parts) - 1
	}
	tool := parts[idx].Tool
	if tool == nil {
		tool = &AssistantTool{}
		parts[idx].Tool = tool
	}
	if result != nil {
		tool.Result, tool.IsError = result, isError
		return parts
	}
	if delta {
		tool.Call.ID = call.ID
		if call.Type != "" {
			tool.Call.Type = call.Type
		}
		if call.Function.Name != "" {
			tool.Call.Function.Name = call.Function.Name
		}
		tool.Call.Function.Arguments += call.Function.Arguments
	} else {
		arguments := tool.Call.Function.Arguments
		tool.Call = call
		if call.Function.Arguments == "" {
			tool.Call.Function.Arguments = arguments
		}
	}
	if definition != nil {
		tool.Definition = *definition
	}
	return parts
}
