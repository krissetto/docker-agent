package chat

import (
	"encoding/json"
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// refEvent models the runtime events that feed the tool branch of
// captureAssistantPresentation (persistence_observer.go) plus text deltas.
type refEvent struct {
	kind       string // "content", "reasoning", "partial", "call", "response"
	text       string
	call       tools.ToolCall
	definition *tools.Tool
	result     *string
	isError    bool
}

// refCapture is a local copy of the runtime's capturePresentation and the tool
// branch of captureAssistantPresentation, used as the reference implementation.
// Definition cloning is replaced by a value copy because pkg/runtime is not importable here.
func refCapture(text *AssistantTextAppender, parts []AssistantPart, e refEvent) []AssistantPart {
	switch e.kind {
	case "content":
		return text.Append(parts, AssistantPart{Type: AssistantPartContent, Text: e.text})
	case "reasoning":
		return text.Append(parts, AssistantPart{Type: AssistantPartReasoning, Text: e.text})
	}
	var call tools.ToolCall
	var definition *tools.Tool
	var result *string
	var isError, delta bool
	switch e.kind {
	case "partial":
		call, definition, delta = e.call, e.definition, true
	case "call":
		call, definition = e.call, e.definition
	case "response":
		call.ID, definition, result = e.call.ID, e.definition, e.result
		isError = e.isError
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
		output := *result
		tool.Result, tool.IsError = &output, isError
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

// refToOp converts a reference event into the op the runtime would persist.
func refToOp(e refEvent) (AssistantPartOp, bool) {
	switch e.kind {
	case "partial":
		def := e.definition
		return AssistantPartOp{Kind: AssistantPartOpToolDelta, ToolCallID: e.call.ID, Type: string(e.call.Type), Name: e.call.Function.Name, Arguments: e.call.Function.Arguments, Definition: def}, true
	case "call":
		call := e.call
		return AssistantPartOp{Kind: AssistantPartOpToolCall, ToolCallID: call.ID, Call: &call, Definition: e.definition}, true
	case "response":
		return AssistantPartOp{Kind: AssistantPartOpToolResult, ToolCallID: e.call.ID, Result: e.result, IsError: e.isError}, true
	}
	return AssistantPartOp{}, false
}

func randomRefEvent(rng *rand.Rand) refEvent {
	ids := []string{"", "t1", "t2", "t3"}
	names := []string{"", "fn_a", "fn_b"}
	fragments := []string{"", "{", "\"x\":", "1", "}"}
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	randomDefinition := func() *tools.Tool {
		if rng.Intn(3) == 0 {
			return nil
		}
		return &tools.Tool{Name: pick(names), Description: pick([]string{"", "desc"}), Category: pick([]string{"", "cat"})}
	}
	switch rng.Intn(7) {
	case 0:
		return refEvent{kind: "content", text: pick([]string{"", "hello ", "world"})}
	case 1:
		return refEvent{kind: "reasoning", text: pick([]string{"", "think ", "more"})}
	case 2, 3:
		call := tools.ToolCall{ID: pick(ids), Type: tools.ToolType(pick([]string{"", "function"})), Function: tools.FunctionCall{Name: pick(names), Arguments: pick(fragments)}}
		return refEvent{kind: "partial", call: call, definition: randomDefinition()}
	case 4:
		call := tools.ToolCall{ID: pick(ids), Type: tools.ToolType(pick([]string{"", "function"})), Function: tools.FunctionCall{Name: pick(names), Arguments: pick([]string{"", "{\"a\":1}"})}}
		return refEvent{kind: "call", call: call, definition: randomDefinition()}
	default:
		result := pick([]string{"", "ok", "boom"})
		return refEvent{kind: "response", call: tools.ToolCall{ID: pick(ids)}, definition: randomDefinition(), result: &result, isError: rng.Intn(2) == 0}
	}
}

func TestApplyAssistantPartOpMatchesReferenceRuntimeAlgorithm(t *testing.T) {
	t.Parallel()
	for seed := range 300 {
		rng := rand.New(rand.NewSource(int64(seed)))
		var refText, opText AssistantTextAppender
		var refParts, opParts []AssistantPart
		for step := range 80 {
			event := randomRefEvent(rng)
			refParts = refCapture(&refText, refParts, event)
			switch event.kind {
			case "content":
				opParts = opText.Append(opParts, AssistantPart{Type: AssistantPartContent, Text: event.text})
			case "reasoning":
				opParts = opText.Append(opParts, AssistantPart{Type: AssistantPartReasoning, Text: event.text})
			default:
				op, ok := refToOp(event)
				require.True(t, ok)
				opParts = ApplyAssistantPartOp(opParts, op)
			}
			require.Equal(t, refParts, opParts, "seed %d step %d event %+v", seed, step, event)
		}
		// Persisted ops round-trip through JSON, so replay must not depend on pointer identity.
		data, err := json.Marshal(refParts)
		require.NoError(t, err)
		var roundTrip []AssistantPart
		require.NoError(t, json.Unmarshal(data, &roundTrip))
		again, err := json.Marshal(roundTrip)
		require.NoError(t, err)
		assert.JSONEqf(t, string(data), string(again), "seed %d", seed)
	}
}

func TestApplyAssistantPartOpToolCallKeepsArgumentsWhenFinalCallOmitsThem(t *testing.T) {
	t.Parallel()
	var parts []AssistantPart
	parts = ApplyAssistantPartOp(parts, AssistantPartOp{Kind: AssistantPartOpToolDelta, ToolCallID: "t", Name: "fn", Arguments: "{\"a\""})
	parts = ApplyAssistantPartOp(parts, AssistantPartOp{Kind: AssistantPartOpToolDelta, ToolCallID: "t", Arguments: ":1}"})
	parts = ApplyAssistantPartOp(parts, AssistantPartOp{Kind: AssistantPartOpToolCall, ToolCallID: "t", Call: &tools.ToolCall{ID: "t", Function: tools.FunctionCall{Name: "fn"}}})
	require.Len(t, parts, 1)
	assert.Equal(t, "{\"a\":1}", parts[0].Tool.Call.Function.Arguments)
}

func TestApplyAssistantPartOpResultNeverCreatesToolPart(t *testing.T) {
	t.Parallel()
	out := "orphan"
	parts := ApplyAssistantPartOp(nil, AssistantPartOp{Kind: AssistantPartOpToolResult, ToolCallID: "missing", Result: &out})
	assert.Nil(t, parts)
	parts = ApplyAssistantPartOp(nil, AssistantPartOp{Kind: AssistantPartOpToolResult, Result: &out})
	assert.Nil(t, parts)
	assert.Nil(t, ApplyAssistantPartOp(nil, AssistantPartOp{Kind: "bogus", ToolCallID: "t"}))
}
