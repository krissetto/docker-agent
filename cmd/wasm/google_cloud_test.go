//go:build js && wasm && docker_agent_wasm_cloud

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

type googleCaptureTransport struct {
	requests []*http.Request
	body     string
}

func (c *googleCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, req)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    req,
	}, nil
}

func TestWasmGoogleFactoryUsesSessionToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		publisher string
		model     string
		endpoint  string
		body      string
	}{
		{"google", "gemini-2.5-flash", ":streamGenerateContent", `data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}]}` + "\n\n"},
		{"anthropic", "claude-sonnet-4-6", ":streamRawPredict", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"meta", "meta/llama-test", "/chat/completions", "data: [DONE]\n\n"},
	} {
		t.Run(tc.publisher, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: "google", Model: tc.model, TokenKey: "SESSION_KEY",
				ProviderOpts: map[string]any{"publisher": tc.publisher, "project": "test-project", "location": "us-central1"}}
			values := map[string]string{"GOOGLE_API_KEY": "unused", vertexTokenEnv: "wrong-key"}
			env := environment.NewMapEnvProvider(values)
			_, err := demoProviders.New(t.Context(), cfg, env)
			require.ErrorContains(t, err, "requires an OAuth access token in the session env (SESSION_KEY)")

			values["SESSION_KEY"] = "initial-token"
			capture := &googleCaptureTransport{body: tc.body}
			client, err := demoProviders.New(t.Context(), cfg, env, options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper { return capture }))
			require.NoError(t, err)
			values["SESSION_KEY"] = "rotated-token"
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			for {
				if _, err := stream.Recv(); err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
			}
			require.Len(t, capture.requests, 1)
			req := capture.requests[0]
			assert.Equal(t, "us-central1-aiplatform.googleapis.com", req.URL.Host)
			assert.True(t, strings.HasSuffix(req.URL.Path, tc.endpoint), req.URL.Path)
			assert.Equal(t, "Bearer rotated-token", req.Header.Get("Authorization"))
			assert.Empty(t, req.Header.Get("X-Api-Key"))
			assert.Empty(t, req.Header.Get("X-Goog-Api-Key"))
		})
	}
}
