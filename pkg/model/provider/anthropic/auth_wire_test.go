package anthropic

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// authHeaders records the credential headers of every request the server sees.
type authHeaders struct {
	mu            sync.Mutex
	authorization []string
	apiKey        []string
}

func (a *authHeaders) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.authorization = append(a.authorization, r.Header.Values("Authorization")...)
		a.apiKey = append(a.apiKey, r.Header.Values("X-Api-Key")...)
		a.mu.Unlock()
		writeMinimalAnthropicSSE(w)
	})
}

// drainAuthRequest sends one chat turn and drains the stream.
func drainAuthRequest(t *testing.T, client *Client) {
	t.Helper()
	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			return
		}
	}
}

// The direct API key path sends the key on x-api-key only, as the Anthropic
// API and v1 expect. Authorization must not carry a second credential.
func TestWire_DirectAPIKeyUsesXApiKeyOnly(t *testing.T) {
	var seen authHeaders
	server := httptest.NewServer(seen.handler(t))
	t.Cleanup(server.Close)

	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6", BaseURL: server.URL}
	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "direct-key"}))
	require.NoError(t, err)

	drainAuthRequest(t, client)

	seen.mu.Lock()
	defer seen.mu.Unlock()
	assert.Equal(t, []string{"direct-key"}, seen.apiKey, "x-api-key carries the API key exactly once")
	assert.Empty(t, seen.authorization, "no Authorization header on the direct API key path")
}

// The gateway path authenticates with one credential: the Docker token is the
// Authorization bearer and also the x-api-key header, exactly once each.
func TestWire_GatewayUsesOneCredentialPerHeader(t *testing.T) {
	var seen authHeaders
	server := httptest.NewServer(seen.handler(t))
	t.Cleanup(server.Close)

	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	env := environment.NewMapEnvProvider(map[string]string{environment.DockerDesktopTokenEnv: "gw-token"})
	client, err := NewClient(t.Context(), cfg, env, options.WithGateway(server.URL))
	require.NoError(t, err)

	drainAuthRequest(t, client)

	seen.mu.Lock()
	defer seen.mu.Unlock()
	assert.Equal(t, []string{"Bearer gw-token"}, seen.authorization, "exactly one Authorization bearer, no duplicate")
	assert.Equal(t, []string{"gw-token"}, seen.apiKey, "exactly one x-api-key with the gateway token")
}
