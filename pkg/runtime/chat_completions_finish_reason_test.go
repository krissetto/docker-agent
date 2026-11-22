package runtime

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

const (
	ccContent   = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial answer"},"finish_reason":null}]}` + "\n\n"
	ccUsageOnly = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n"
	ccDone      = "data: [DONE]\n\n"
)

func ccFinish(reason string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}` + "\n\n"
}

// newChatCompletionsClient returns a real *openai.Client on the Chat
// Completions API (track_usage at its default) serving sse, and a counter of
// the requests it received.
func newChatCompletionsClient(t *testing.T, sse string) (*openai.Client, *atomic.Int64) {
	t.Helper()

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)

	cfg := latest.ModelConfig{
		Provider:     "openai",
		Model:        "gpt-4o",
		BaseURL:      srv.URL,
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}
	client, err := openai.NewClient(t.Context(), &cfg,
		environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}))
	require.NoError(t, err)
	return client, &requests
}

// runChatCompletionsTurn runs one turn and returns the last assistant
// message's finish reason and the last per-message usage reported. An empty
// refused turn is not persisted, so its finish reason is read from the
// boundary message the runtime emits for it.
func runChatCompletionsTurn(t *testing.T, rt *LocalRuntime, sess *session.Session) (chat.FinishReason, *MessageUsage) {
	t.Helper()

	var stopped bool
	var last *MessageUsage
	var added chat.FinishReason
	for ev := range rt.runExecution(t.Context(), sess) {
		switch e := ev.(type) {
		case *ErrorEvent:
			t.Fatalf("unexpected error event: %s", e.Error)
		case *StreamStoppedEvent:
			stopped = true
		case *TokenUsageEvent:
			if e.Usage != nil && e.Usage.LastMessage != nil {
				last = e.Usage.LastMessage
			}
		case *MessageAddedEvent:
			if e.Message != nil && e.Message.Message.Role == chat.MessageRoleAssistant {
				added = e.Message.Message.FinishReason
			}
		}
	}
	require.True(t, stopped, "the stream must stop")

	var persisted chat.FinishReason
	for _, msg := range sess.OwnMessages() {
		if msg.Message.Role == chat.MessageRoleAssistant {
			persisted = msg.Message.FinishReason
		}
	}
	if persisted == "" {
		return added, last
	}
	return persisted, last
}

// TestRunStream_ChatCompletionsFinishReason drives a real *openai.Client on
// the Chat Completions API (the path DMR and OpenAI-compatible providers
// use) through LocalRuntime. A provider "length" must reach the persisted
// turn and its usage as length, so truncation is reported, and a stream with
// no usage chunk must still carry the provider's reason.
func TestRunStream_ChatCompletionsFinishReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sse        string
		want       chat.FinishReason
		wantOutput int64
	}{
		{"length with usage chunk", ccContent + ccFinish("length") + ccUsageOnly + ccDone, chat.FinishReasonLength, 3},
		{"length without usage chunk", ccContent + ccFinish("length") + ccDone, chat.FinishReasonLength, 0},
		{"stop with usage chunk", ccContent + ccFinish("stop") + ccUsageOnly + ccDone, chat.FinishReasonStop, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, _ := newChatCompletionsClient(t, tt.sse)
			root := agent.New("root", "test", agent.WithModel(client))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			sess := session.New(session.WithUserMessage("hi"))
			sess.NonInteractive = true

			finish, last := runChatCompletionsTurn(t, rt, sess)
			assert.Equal(t, tt.want, finish)
			if tt.wantOutput > 0 {
				require.NotNil(t, last)
				assert.Equal(t, tt.want, last.FinishReason)
				assert.Equal(t, tt.wantOutput, last.OutputTokens)
			}
		})
	}
}
