package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestRemoteDelegationPolicyPersistsAndIsTreeScoped(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	srv, owner := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	first, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	second, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":false,"unknown":true}`, `{"enabled":"false"}`} {
		invalid := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/"+first.ID()+"/delegation-policy", body, "")
		require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())
	}
	controller, ok := first.(runtime.SessionDelegationController)
	require.True(t, ok)
	require.True(t, first.Metadata().Capabilities.DelegationPolicy)
	before, err := second.(runtime.SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, controller.SetDelegationPolicy(t.Context(), false))
	enabled, err := controller.DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
	unaffected, err := second.(runtime.SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, unaffected)
	persisted, err := store.GetSession(t.Context(), first.ID())
	require.NoError(t, err)
	require.Equal(t, "false", persisted.AttributesSnapshot()[runtime.SessionDelegationAttribute])
	require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context())))
	restoredSrv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	restoredHTTP := httptest.NewServer(restoredSrv.e)
	defer restoredHTTP.Close()
	restoredClient, err := runtime.NewClient(restoredHTTP.URL)
	require.NoError(t, err)
	restoredTransport, err := runtime.NewSessionTransport(restoredClient)
	require.NoError(t, err)
	restored, _, err := restoredTransport.LoadSession(t.Context(), first.ID())
	require.NoError(t, err)
	enabled, err = restored.(runtime.SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
}

func TestDelegationPolicyHTTPAuthorizationAndUnsupported(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"root": {id: "root", agent: "root"}}}
	srv, _ := newSessionHTTPServer(t, registry)
	unsupported := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/root/delegation-policy", "", "")
	require.Equal(t, http.StatusNotImplemented, unsupported.Code)
	srv = NewWithManager(srv.sm, "secret")
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		denied := sessionRequest(t, srv, method, "/api/v2/sessions/root/delegation-policy", `{"enabled":false}`, "")
		require.Equal(t, http.StatusUnauthorized, denied.Code)
	}
}
