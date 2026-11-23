//go:build js && wasm

package main

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

type toolCallTestModel struct {
	provider.Provider
	stream chat.MessageStream
}

func (m toolCallTestModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return m.stream, nil
}

type toolCallTestStream struct {
	calls []tools.ToolCall
}

func (s *toolCallTestStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.calls) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	call := s.calls[0]
	s.calls = s.calls[1:]
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{call}}}}}, nil
}

func (s *toolCallTestStream) Close() {}

func TestToolCallPreservesProviderID(t *testing.T) {
	t.Parallel()
	rt := &wasmRuntime{}
	model := toolCallTestModel{stream: &toolCallTestStream{calls: []tools.ToolCall{
		{ID: "local-call", Type: "function", Function: tools.FunctionCall{Name: "echo"}},
		{ID: "local-call", ProviderID: "gemini-call", Function: tools.FunctionCall{Arguments: `{"text":`}},
		{ID: "local-call", Function: tools.FunctionCall{Arguments: `"Paris"}`}},
	}}}
	result, err := rt.streamCompletion(t.Context(), model, nil, nil)
	require.NoError(t, err)
	require.Len(t, result.toolCalls, 1)
	assert.Equal(t, "gemini-call", result.toolCalls[0].ProviderID)
	assert.JSONEq(t, `{"text":"Paris"}`, result.toolCalls[0].Function.Arguments)
}
