package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRemoteCreateCompatibleWithStrictOlderV2(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.SessionAPIPath {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		// An old peer accepts the historical create schema, not session_id.
		var old struct {
			AgentName string `json:"agent_name"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&old); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.SessionMetadata{SessionID: "server-allocated", AgentName: old.AgentName})
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL)
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	h, err := transport.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.Equal(t, "server-allocated", h.ID())
	require.EqualValues(t, 1, posts.Load())
	_, err = transport.CreateSessionWithID(t.Context(), session.New(), SessionBinding{AgentName: "root"}, "desired")
	var typed *SessionError
	require.ErrorAs(t, err, &typed)
	require.Equal(t, SessionErrorUnsupported, typed.Kind)
	require.EqualValues(t, 1, posts.Load(), "unsupported explicit identity must never issue a mutation")
}
