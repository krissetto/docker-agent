package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestStopSubtreeHTTPPreservesSessionAndRequiresAuthorization(t *testing.T) {
	handle := &httpSession{id: "child", agent: "worker", stopSubtree: true}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"child": handle}}
	srv, store := newSessionHTTPServer(t, registry)
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("child"), session.WithAgentName("worker"))))
	srv.sm.runtimeSessions.Store("child", &activeRuntimes{handle: handle, registry: registry})
	metadata := sessionMetadata(runtime.SessionMetadata{SessionID: "child", Capabilities: runtime.SessionCapabilities{StopSubtree: true}})
	endpoint := "/api/v2/sessions/" + metadata.SessionID + "/stop-subtree"
	stopped := sessionRequest(t, srv, http.MethodPost, endpoint, "", "")
	require.Equal(t, http.StatusNoContent, stopped.Code, stopped.Body.String())
	persisted, err := store.GetSession(t.Context(), metadata.SessionID)
	require.NoError(t, err)
	require.Equal(t, metadata.SessionID, persisted.ID)
	require.Equal(t, 1, handle.stops)
	status := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+metadata.SessionID+"/status", "", "")
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())
	srv = NewWithManager(srv.sm, "secret")
	denied := sessionRequest(t, srv, http.MethodPost, endpoint, "", "")
	require.Equal(t, http.StatusUnauthorized, denied.Code)
}

func TestStopSubtreeHTTPRejectsUnsupportedRoot(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"root": {id: "root", agent: "root"}}}
	srv, _ := newSessionHTTPServer(t, registry)
	response := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/root/stop-subtree", "", "")
	require.Equal(t, http.StatusNotImplemented, response.Code, response.Body.String())
}

func TestInterruptedTurnStatusProjection(t *testing.T) {
	status := runtime.SessionStatus{SessionID: "interrupted", InterruptedTurns: 2}
	require.Equal(t, 2, sessionStatus(status).InterruptedTurns)
	snapshot := sessionSnapshot(runtime.SessionSnapshot{Status: status})
	require.Equal(t, 2, snapshot.Status.InterruptedTurns)
}
