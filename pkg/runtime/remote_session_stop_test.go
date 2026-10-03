package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
)

func TestRemoteStopSubtreeUsesNondestructiveEndpoint(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, api.SessionAPIPath+"/root/stop-subtree", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle := &remoteSession{runtime: transport, sessionID: "root", metadata: SessionMetadata{Capabilities: SessionCapabilities{StopSubtree: true}}}
	require.NoError(t, handle.StopSubtree(t.Context()))
	require.Equal(t, 1, requests)
	handle.metadata.Capabilities.StopSubtree = false
	require.ErrorIs(t, handle.StopSubtree(t.Context()), &SessionError{Kind: SessionErrorUnsupported})
	require.Equal(t, 1, requests)
}

func TestRemoteSessionStartPreservesStableIdentitiesAndSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, api.SessionAPIPath+"/start", r.URL.Path)
		var request api.SessionStartRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "team", request.Source)
		assert.Equal(t, "stable", request.SessionID)
		assert.Equal(t, "first", request.Input.RequestID)
		fmt.Fprint(w, `{"session_id":"stable","turn_id":"accepted"}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client, WithSessionTransportSource("team"))
	require.NoError(t, err)
	submission, err := transport.StartSession(t.Context(), api.SessionStartRequest{SessionCreateRequest: api.SessionCreateRequest{SessionID: "stable", AgentName: "root"}, Input: api.SessionInputRequest{RequestID: "first", Content: "hello"}})
	require.NoError(t, err)
	require.Equal(t, "stable", submission.SessionID)
	require.Equal(t, "accepted", submission.TurnID)
}

func TestRemoteDelegationPolicyUnsupportedDoesNotContactServer(t *testing.T) {
	handle := &remoteSession{sessionID: "root"}
	_, err := handle.DelegationPolicy(t.Context())
	require.ErrorIs(t, err, &SessionError{Kind: SessionErrorUnsupported})
	require.ErrorIs(t, handle.SetDelegationPolicy(t.Context(), false), &SessionError{Kind: SessionErrorUnsupported})
}
