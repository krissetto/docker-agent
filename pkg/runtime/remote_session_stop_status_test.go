package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
)

// The remote client must surface the HTTP status's explicit stop flag.
func TestRemoteStatusDecodesExplicitStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, api.SessionAPIPath+"/child/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"metadata": api.SessionMetadata{SessionID: "child"},
			"status":   api.SessionStatus[SessionState]{SessionID: "child", State: SessionStateSettled, Stopped: true},
		}))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("child")
	require.NoError(t, err)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Stopped)
}
