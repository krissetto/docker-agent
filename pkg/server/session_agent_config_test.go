package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCanonicalSessionAgentConfigAuthenticatedAndBound(t *testing.T) {
	srv, _ := newPortableServer(t)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithAuthToken("token"), runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	h, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	endpoint := api.SessionAPIPath + "/" + h.ID() + "/agent-config"
	require.Equal(t, http.StatusUnauthorized, sessionRequest(t, srv, http.MethodGet, endpoint+"?agent=root", "", "").Code)
	require.Equal(t, http.StatusBadRequest, sessionRequest(t, srv, http.MethodGet, endpoint, "", "token").Code)
	require.Equal(t, http.StatusNotFound, sessionRequest(t, srv, http.MethodGet, endpoint+"?agent=foreign", "", "token").Code)
	response := sessionRequest(t, srv, http.MethodGet, endpoint+"?agent=root", "", "token")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var local runtime.SessionAgentConfig
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &local))
	remote, err := h.(runtime.SessionAgentConfigReader).SessionAgentConfig(t.Context(), "root")
	require.NoError(t, err)
	require.Equal(t, local, remote)
	require.Equal(t, h.ID(), remote.SessionID)
	require.True(t, remote.Info.IsCurrent)
	require.NotEmpty(t, remote.Info.Toolsets)
}
