package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestSessionViewRemoteConfirmedPrepareCommitAndResume(t *testing.T) {
	store := session.NewInMemorySessionStore()
	archived := session.New(session.WithID("archived"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	archived.CreatedAt = time.Unix(100, 0).UTC()
	archived.WorkingDir = "/synthetic/workspace"
	input := session.UserMessage("archived")
	input.TurnID, input.Pending, input.Accepted = "accepted", true, true
	archived.AddMessage(input)
	require.NoError(t, store.AddSession(t.Context(), archived))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(archived.WorkingDir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(owner.Runtime()))
	handler := NewWithManager(sm, "")
	server := httptest.NewServer(handler.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	prepared, err := transport.PrepareSessionView(t.Context(), archived.ID)
	require.NoError(t, err)
	info := prepared.Info()
	assert.Equal(t, archived.ID, info.SessionID)
	assert.Equal(t, archived.WorkingDir, info.WorkingDir)
	_, err = owner.Runtime().SessionByID(archived.ID)
	require.Error(t, err, "confirmed-info never publishes owner")
	info.Session.SetTitle("mutated")
	assert.NotEqual(t, "mutated", prepared.Info().Session.TitleSnapshot())
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	status, err := committed.SessionHandle.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Dormant)
	assert.Equal(t, 1, status.Pending)
	assert.Equal(t, archived.WorkingDir, committed.Info.WorkingDir)
	prepared.Abort()
	_, err = committed.SessionHandle.Edit(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditResume})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, committed.SessionHandle.AwaitTurn(ctx, "accepted"))
	status, err = committed.SessionHandle.Status(t.Context())
	require.NoError(t, err)
	assert.False(t, status.Dormant)
}

func TestSessionViewUnknownPrepareViewFailsBeforeColdHandle(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("cold"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	require.NoError(t, base.AddSession(t.Context(), root))
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	sm := NewSessionManager(t.Context(), nil, base, 0, nil, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "secret")
	for _, suffix := range []string{"?view=", "?view=unknown", "?view=prepare-info&view=unknown"} {
		response := sessionRequest(t, srv, http.MethodGet, "/api/sessions/cold"+suffix, "", "secret")
		assert.Equal(t, http.StatusBadRequest, response.Code)
	}
	assert.Zero(t, registry.createCount)
	unauthorized := sessionRequest(t, srv, http.MethodGet, "/api/sessions/cold?view=prepare-info", "", "")
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	assert.Zero(t, registry.createCount)
}

func TestSessionViewRemoteLegacyPrepareDoesNotCommitFallback(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		if !assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session_id": "old"})) {
			return
		}
	}))
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	_, err = transport.PrepareSessionView(t.Context(), "old")
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}
