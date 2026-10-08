package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIResponseClone(t *testing.T) {
	t.Parallel()
	reasoning := "visible"
	original := &OpenAIResponse{Source: "openai/gpt-5.6", ReasoningContent: &reasoning, Output: []json.RawMessage{json.RawMessage(`{"encrypted_content":"opaque"}`)}}
	cloned := original.Clone()
	*cloned.ReasoningContent = "changed"
	cloned.Output[0][0] = '['
	require.Equal(t, "visible", *original.ReasoningContent)
	require.Equal(t, byte('{'), original.Output[0][0])
	require.Nil(t, (*OpenAIResponse)(nil).Clone())
	require.Nil(t, (&OpenAIResponse{}).Clone().Output)
}
