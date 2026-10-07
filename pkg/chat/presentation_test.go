package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAssistantPresentationPreservesFirstAppearance(t *testing.T) {
	var parts []AssistantPart
	for _, part := range []AssistantPart{
		{Type: AssistantPartReasoning, Text: "first"},
		{Type: AssistantPartReasoning, Text: " thought"},
		{Type: AssistantPartToolCall, ToolCallID: "a"},
		{Type: AssistantPartReasoning, Text: "next"},
		{Type: AssistantPartContent, Text: "commentary"},
		{Type: AssistantPartToolCall, ToolCallID: "a"},
		{Type: AssistantPartContent, Text: " continued"},
		{Type: AssistantPartToolCall, ToolCallID: "b"},
		{Type: AssistantPartReasoning},
		{Type: AssistantPartToolCall},
	} {
		parts = AppendAssistantPart(parts, part)
	}
	require.Equal(t, []AssistantPart{
		{Type: AssistantPartReasoning, Text: "first thought"},
		{Type: AssistantPartToolCall, ToolCallID: "a"},
		{Type: AssistantPartReasoning, Text: "next"},
		{Type: AssistantPartContent, Text: "commentary continued"},
		{Type: AssistantPartToolCall, ToolCallID: "b"},
	}, parts)
}

func TestAssistantPresentationJSONCompatibility(t *testing.T) {
	var legacy Message
	require.NoError(t, json.Unmarshal([]byte(`{"role":"assistant","content":"answer","reasoning_content":"thought"}`), &legacy))
	require.Nil(t, legacy.Presentation, "missing legacy order must not be inferred")
	legacyJSON, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(legacyJSON), "presentation")

	ordered := legacy
	ordered.Presentation = []AssistantPart{
		{Type: AssistantPartContent, Text: "answer"},
		{Type: AssistantPartToolCall, ToolCallID: "call"},
		{Type: AssistantPartReasoning, Text: "thought"},
	}
	encoded, err := json.Marshal(ordered)
	require.NoError(t, err)
	var decoded Message
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, ordered, decoded)
}
