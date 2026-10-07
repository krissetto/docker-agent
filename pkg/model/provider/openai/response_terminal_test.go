package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// terminalBarrierStream leaves the transport open after semantic completion.
type terminalBarrierStream struct {
	fakeEventStream
	attempted chan struct{}
	release   chan struct{}
	closed    bool
}

func (s *terminalBarrierStream) Next() bool {
	if s.pos == len(s.events) {
		close(s.attempted)
		<-s.release
		return false
	}
	return s.fakeEventStream.Next()
}
func (s *terminalBarrierStream) Close() error { s.closed = true; return nil }

func TestResponseStream_CompletedDoesNotWaitForTransportEOF(t *testing.T) {
	for _, terminalType := range []string{"response.completed", "response.done"} {
		t.Run(terminalType, func(t *testing.T) {
			terminal := toolCallsCompletedEvent("resp_todo_terminal")
			terminal["type"] = terminalType
			stream := &terminalBarrierStream{
				fakeEventStream: fakeEventStream{events: decodeEvents(t, []map[string]any{
					{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item_todo", "call_id": "call_todo", "name": "create_todo"}},
					{"type": "response.function_call_arguments.done", "item_id": "item_todo", "arguments": `{"description":"short"}`},
					terminal,
				})},
				attempted: make(chan struct{}), release: make(chan struct{}),
			}
			defer close(stream.release)
			adapter := newResponseStreamAdapter(stream, true)
			defer func() { adapter.Close(); assert.True(t, stream.closed) }()

			name, err := adapter.Recv()
			require.NoError(t, err)
			require.Len(t, name.Choices, 1)
			require.Len(t, name.Choices[0].Delta.ToolCalls, 1)
			assert.Equal(t, "call_todo", name.Choices[0].Delta.ToolCalls[0].ID)
			assert.Equal(t, "create_todo", name.Choices[0].Delta.ToolCalls[0].Function.Name)
			args, err := adapter.Recv()
			require.NoError(t, err)
			require.Len(t, args.Choices, 1)
			require.Len(t, args.Choices[0].Delta.ToolCalls, 1)
			assert.Equal(t, "call_todo", args.Choices[0].Delta.ToolCalls[0].ID)
			assert.JSONEq(t, `{"description":"short"}`, args.Choices[0].Delta.ToolCalls[0].Function.Arguments)
			final, err := adapter.Recv()
			require.NoError(t, err)
			require.Len(t, final.Choices, 1)
			assert.Equal(t, chat.FinishReasonToolCalls, final.Choices[0].FinishReason)
			require.NotNil(t, final.Usage)
			assert.Equal(t, int64(8), final.Usage.InputTokens)
			assert.Equal(t, int64(12), final.Usage.OutputTokens)

			returned := make(chan error, 1)
			go func() { _, err := adapter.Recv(); returned <- err }()
			select {
			case err := <-returned:
				require.ErrorIs(t, err, io.EOF)
				_, err = adapter.Recv()
				require.ErrorIs(t, err, io.EOF)
			case <-stream.attempted:
				// Release the blocked reader before reporting the regression.
				stream.release <- struct{}{}
				require.ErrorIs(t, <-returned, io.EOF)
				t.Error("semantic completion read the transport again")
			}
		})
	}
}

func TestResponseStream_CompletedPreservesPooledConnection(t *testing.T) {
	srv, captured := testWSServerCapture(t, []map[string]any{completedEvent("resp_reused")})
	defer srv.Close()
	pool := newWSPool("ws"+strings.TrimPrefix(srv.URL, "http"), func(context.Context) (http.Header, error) { return http.Header{}, nil })
	defer pool.Close()
	var firstConnection *wsConnection
	for i := range 2 {
		stream, err := pool.Stream(t.Context(), defaultTestParams())
		require.NoError(t, err)
		adapter := newResponseStreamAdapter(stream, true)
		final, err := adapter.Recv()
		require.NoError(t, err)
		require.NotNil(t, final.Usage)
		_, err = adapter.Recv()
		require.ErrorIs(t, err, io.EOF)
		adapter.Close()
		require.Equal(t, "resp_reused", pool.lastResponseID)
		require.NotNil(t, pool.conn)
		if i == 0 {
			firstConnection = pool.conn
		} else {
			require.Same(t, firstConnection, pool.conn)
		}
	}
	require.Len(t, *captured, 2)
	assertPreviousResponseID(t, (*captured)[0], "")
	assertPreviousResponseID(t, (*captured)[1], "resp_reused")
}
