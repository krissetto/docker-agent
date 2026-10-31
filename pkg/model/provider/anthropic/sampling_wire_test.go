package anthropic

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// Sampling options built for the Beta API must reach both the initial
// request and the context-length retry, which is rebuilt from the same params.
func TestBetaSamplingReachesInitialAndRetryRequests(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		messages []capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":1000000}`))
			return
		}
		req := capturedRequest{path: r.URL.Path, raw: string(raw)}
		_ = json.Unmarshal(raw, &req.body)
		mu.Lock()
		messages = append(messages, req)
		first := len(messages) == 1
		mu.Unlock()
		if first {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(textReply("msg_retry", "ok")))
	}))
	t.Cleanup(srv.Close)

	client := newScriptedClient(&scriptedServer{Server: srv}, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-sonnet-4-6",
		Temperature: new(0.2), TopP: new(0.9),
		ProviderOpts: map[string]any{"top_k": 5, "fallbacks": []string{"claude-haiku-4-5"}},
	})
	msg := chatTurn(t, client, []chat.Message{user("hello")}, nil)
	assert.Equal(t, "ok", msg.Content)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, messages, 2, "the context-length error must trigger exactly one retry")
	for i, req := range messages {
		assert.Equal(t, "/v1/messages", req.path)
		assert.InDelta(t, 0.2, req.body["temperature"], 1e-9, "request %d", i)
		assert.InDelta(t, 0.9, req.body["top_p"], 1e-9, "request %d", i)
		assert.InDelta(t, 5, req.body["top_k"], 0, "request %d", i)
	}
	assert.InDelta(t, 1, messages[1].body["max_tokens"], 0, "the retry clamps max_tokens")
}
