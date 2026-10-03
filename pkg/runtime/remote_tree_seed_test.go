package runtime

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func observeRemoteTreeSeeds(t *testing.T) Observation {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "true", r.URL.Query().Get("tree")) {
			http.Error(w, "missing tree query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		out := bufio.NewWriter(w)
		frames := []string{
			`{"version":2,"type":"snapshot","snapshot":{"session":{"id":"root"},"status":{"session_id":"root","agent_name":"root","state":"running","pending":0},"interactions":[],"pending_inputs":[],"cursor":4,"transcript_position":0}}`,
			`{"version":2,"type":"snapshot","snapshot":{"session":{"id":"child"},"status":{"session_id":"child","agent_name":"worker","state":"running","pending":0},"interactions":[],"pending_inputs":[],"cursor":9,"transcript_position":0}}`,
			`{"version":2,"type":"event","envelope":{"version":2,"session_id":"root","sequence":0,"transcript_position":-1,"event":{"type":"stream_started","session_id":"root","agent_name":"root"}}}`,
			`{"version":2,"type":"event","envelope":{"version":2,"session_id":"child","sequence":0,"transcript_position":-1,"event":{"type":"stream_started","session_id":"child","agent_name":"worker"}}}`,
			`{"version":2,"type":"ready"}`,
		}
		for _, frame := range frames {
			fmt.Fprintf(out, "data: %s\n\n", frame)
		}
		if !assert.NoError(t, out.Flush()) {
			return
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("root")
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Tree: true})
	require.NoError(t, err)
	require.NotNil(t, observation.SessionsAdded)
	t.Cleanup(observation.Cancel)
	return observation
}

func TestRemoteTreeAlreadyRunningRootSeed(t *testing.T) {
	observation := observeRemoteTreeSeeds(t)
	require.Len(t, observation.Initial, 2)
	require.Len(t, observation.Replay, 2)
	assert.Equal(t, "root", observation.Replay[0].SessionID)
	assert.Zero(t, observation.Replay[0].Sequence)
}

func TestRemoteTreeAlreadyRunningDescendantSeed(t *testing.T) {
	observation := observeRemoteTreeSeeds(t)
	require.Len(t, observation.Initial, 2)
	require.Len(t, observation.Replay, 2)
	assert.Equal(t, "child", observation.Replay[1].SessionID)
	assert.Zero(t, observation.Replay[1].Sequence)
}
