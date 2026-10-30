package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
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

// stoppedStatusHTTPHandle reports an explicitly stopped root, as a driver does
// after an intentional stop-subtree.
type stoppedStatusHTTPHandle struct {
	httpSession
}

func (h *stoppedStatusHTTPHandle) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{SessionID: h.id, AgentName: h.agent, State: runtime.SessionStateSettled, Stopped: true}, nil
}

func TestStoppedStatusProjection(t *testing.T) {
	status := runtime.SessionStatus{SessionID: "stopped", State: runtime.SessionStateSettled, Stopped: true}
	require.True(t, sessionStatus(status).Stopped)
	require.True(t, sessionSnapshot(runtime.SessionSnapshot{Status: status}).Status.Stopped)
	encoded, err := json.Marshal(sessionStatus(status))
	require.NoError(t, err)
	require.JSONEq(t, `{"session_id":"stopped","agent_name":"","state":"settled","pending":0,"stopped":true}`, string(encoded))
	plain, err := json.Marshal(sessionStatus(runtime.SessionStatus{SessionID: "plain", State: runtime.SessionStateSettled}))
	require.NoError(t, err)
	require.NotContains(t, string(plain), `"stopped"`)
}

// stoppedStatusRegistry resolves the stopped root to the handle under test, so
// the HTTP path reads its explicit stop flag rather than the plain fixture's.
type stoppedStatusRegistry struct {
	*httpSessionRegistry

	handle runtime.SessionHandle
}

func (r *stoppedStatusRegistry) SessionByID(id string) (runtime.SessionHandle, error) {
	if id == r.handle.ID() {
		return r.handle, nil
	}
	return r.httpSessionRegistry.SessionByID(id)
}

func TestStoppedStatusHTTPExposesStop(t *testing.T) {
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("stopped-root"), session.WithAgentName("root"))))
	base := &httpSessionRegistry{sessions: map[string]*httpSession{}, store: store}
	registry := &stoppedStatusRegistry{httpSessionRegistry: base, handle: &stoppedStatusHTTPHandle{httpSession: httpSession{id: "stopped-root", agent: "root"}}}
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "")
	sm.runtimeSessions.Store("stopped-root", &activeRuntimes{handle: registry.handle, registry: registry})
	rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/stopped-root/status", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Status struct {
			Stopped bool `json:"stopped"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.Status.Stopped, rec.Body.String())
}
