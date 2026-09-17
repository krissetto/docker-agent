package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type activeCatalogStore struct{ session.Store }

func (activeCatalogStore) GetSessions(context.Context) ([]*session.Session, error) {
	return nil, errors.New("historical sessions must not be read")
}

func TestSessionHTTPCatalogActiveSkipsHistoricalStore(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	store := activeCatalogStore{session.NewInMemorySessionStore()}
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(registry))
	sess := session.New(session.WithID("active"), session.WithAgentName("root"))
	handle, err := registry.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{handle: handle, registry: registry})
	response := sessionRequest(t, NewWithManager(sm, ""), http.MethodGet, "/api/v2/sessions?active=true", "", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var catalog sessionCatalogDTO
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &catalog))
	require.Len(t, catalog.Sessions, 1)
	require.Equal(t, sess.ID, catalog.Sessions[0].SessionID)
	require.True(t, catalog.Sessions[0].Loaded)
}
