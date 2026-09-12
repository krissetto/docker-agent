package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chatserver"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestServeChatConversationFailedTurnDoesNotAdvanceCache(t *testing.T) {
	modelServer := newRecordingChatCompletionsServer(t)

	agentFile := filepath.Join(t.TempDir(), "agent.yaml")
	modelName := "e2e-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	agentYAML := fmt.Appendf(nil, `version: "9"

providers:
  %[2]s:
    api_type: openai_chatcompletions
    base_url: %[1]s/v1

models:
  %[2]s:
    provider: %[2]s
    model: %[2]s
    max_tokens: 64

agents:
  root:
    model: %[2]s
    description: E2E chat server agent
    instruction: Reply concisely.
`, modelServer.URL(), modelName)
	require.NoError(t, os.WriteFile(agentFile, agentYAML, 0o644))

	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runConfig := &config.RuntimeConfig{
		Config: config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &testEnvProvider{
			environment.DockerDesktopTokenEnv: "DUMMY",
			"OPENAI_API_KEY":                  "DUMMY",
		},
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- chatserver.Run(ctx, agentFile, chatserver.Options{
			RunConfig:                runConfig,
			ConversationsMaxSessions: 10,
			RequestTimeout:           2 * time.Second,
		}, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	waitForChatServer(t, baseURL)
	defer func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				require.NoError(t, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("chat server did not stop")
		}
	}()

	convID := "e2e-failed-turn"
	postChatCompletion(t, baseURL, convID, http.StatusOK, "first")
	postChatCompletion(t, baseURL, convID, http.StatusInternalServerError, "please fail")
	postChatCompletion(t, baseURL, convID, http.StatusOK, "after failure")

	requests := modelServer.requests()
	require.GreaterOrEqual(t, len(requests), 3)
	assert.Equal(t, []string{"first"}, requests[0].userMessages())
	for _, req := range requests[1 : len(requests)-1] {
		assert.Equal(t, []string{"first", "please fail"}, req.userMessages())
	}

	// This is the end-to-end assertion for #2890: the failed "please fail"
	// turn must not have been committed to the X-Conversation-Id cache, so the
	// following successful turn resumes from the last successful state.
	assert.Equal(t, []string{"first", "after failure"}, requests[len(requests)-1].userMessages())
}

type recordingChatCompletionsServer struct {
	server *httptest.Server
	mu     sync.Mutex
	reqs   []recordedChatCompletionRequest
}

type recordedChatCompletionRequest struct {
	Messages []recordedChatMessage `json:"messages"`
}

type recordedChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func newRecordingChatCompletionsServer(t *testing.T) *recordingChatCompletionsServer {
	t.Helper()

	rec := &recordingChatCompletionsServer{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}

		var req recordedChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		rec.mu.Lock()
		rec.reqs = append(rec.reqs, req)
		rec.mu.Unlock()

		if lastUserMessage(req.Messages) == "please fail" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"forced failure","type":"server_error"}}`)
			return
		}

		writeChatCompletionsStream(w, "ok: "+lastUserMessage(req.Messages))
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (s *recordingChatCompletionsServer) requests() []recordedChatCompletionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedChatCompletionRequest(nil), s.reqs...)
}

func (s *recordingChatCompletionsServer) URL() string {
	return s.server.URL
}

func (r recordedChatCompletionRequest) userMessages() []string {
	var out []string
	for _, msg := range r.Messages {
		if msg.Role == "user" {
			out = append(out, messageContentText(msg.Content))
		}
	}
	return out
}

func lastUserMessage(messages []recordedChatMessage) string {
	for _, message := range slices.Backward(messages) {
		if message.Role == "user" {
			return messageContentText(message.Content)
		}
	}
	return ""
}

func messageContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text, ok := m["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func writeChatCompletionsStream(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": content},
		}},
	})
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEData(w http.ResponseWriter, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func waitForChatServer(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/v1/models", http.NoBody)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 50*time.Millisecond)
}

func postChatCompletion(t *testing.T, baseURL, conversationID string, expectedStatus int, userMessage string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "root",
		"messages": []map[string]string{{
			"role":    "user",
			"content": userMessage,
		}},
	})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Conversation-Id", conversationID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, expectedStatus, resp.StatusCode, "response body: %s", string(responseBody))
}
