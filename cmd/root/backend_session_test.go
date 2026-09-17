package root

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestRemoteBackendPassesAgentFileAsSessionSource(t *testing.T) {
	var source, agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v2/sessions", r.URL.Path)
		var request struct {
			Source    string `json:"source"`
			AgentName string `json:"agent_name"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		source, agent = request.Source, request.AgentName
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"session_id":"remote","agent_name":"worker","capabilities":{"per_turn_model":false}}`)
	}))
	defer server.Close()

	flags := &runExecFlags{remoteAddress: server.URL}
	backend := &remoteBackend{flags: flags, agentFileName: "second.yaml"}
	rt, sessions, sess, cleanup, err := backend.CreateSession(t.Context(), nil, runtime.CreateSessionRequest{AgentName: "worker"})
	require.NoError(t, err)
	defer cleanup()
	assert.NotNil(t, rt)
	assert.NotNil(t, sessions)
	assert.Equal(t, "remote", sess.ID)
	assert.Equal(t, "second.yaml", source)
	assert.Equal(t, "worker", agent)
}
