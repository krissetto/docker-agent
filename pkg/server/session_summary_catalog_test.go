package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type summaryOnlyServerStore struct {
	session.Store
	session.ScopedSummaryStore

	reads atomic.Int32
}

func (s *summaryOnlyServerStore) GetSessions(context.Context) ([]*session.Session, error) {
	return nil, errors.New("summary must not load session transcripts")
}

func (s *summaryOnlyServerStore) GetSession(context.Context, string) (*session.Session, error) {
	return nil, errors.New("summary must not perform per-row hydration")
}

func (s *summaryOnlyServerStore) GetSessionSummariesWithScope(ctx context.Context, scope session.SummaryScope) ([]session.Summary, error) {
	s.reads.Add(1)
	return s.ScopedSummaryStore.GetSessionSummariesWithScope(ctx, scope)
}

func TestSessionSummaryCatalogHTTPMetadataScopeAuthAndTypedParity(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}), session.WithUserMessage("private transcript must not be read"))
	root.CreatedAt = time.Unix(100, 0).UTC()
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}), session.WithUserMessage("private child transcript"))
	child.CreatedAt = time.Unix(200, 0).UTC()
	child.AgentModelOverrides = map[string]string{"worker": "provider/model"}
	orphan := session.New(session.WithID("orphan"), session.WithParentID("missing"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
	orphan.CreatedAt = time.Unix(300, 0).UTC()
	for _, sess := range []*session.Session{root, child, orphan} {
		require.NoError(t, base.AddSession(t.Context(), sess))
	}
	store := &summaryOnlyServerStore{Store: base, ScopedSummaryStore: base.(session.ScopedSummaryStore)}
	registry := &httpTreeRegistry{httpSessionRegistry: &httpSessionRegistry{sessions: map[string]*httpSession{}}}
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "secret")
	path := "/api/sessions?view=summary&include_children=true"
	for _, token := range []string{"", "wrong"} {
		response := sessionRequest(t, srv, http.MethodGet, path, "", token)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	}
	assert.Zero(t, store.reads.Load(), "auth rejection happens before metadata access")
	response := sessionRequest(t, srv, http.MethodGet, path, "", "secret")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "private")
	assert.NotContains(t, response.Body.String(), `"messages"`)
	var catalog struct {
		Version  int                           `json:"version"`
		View     string                        `json:"view"`
		Sessions []runtime.SessionSummaryEntry `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &catalog))
	assert.Equal(t, "summary", catalog.View)
	require.Len(t, catalog.Sessions, 3)
	byID := map[string]runtime.SessionSummaryEntry{}
	for _, row := range catalog.Sessions {
		byID[row.SessionID] = row
	}
	assert.True(t, byID[root.ID].Loadable)
	assert.True(t, byID[child.ID].RequiresConfirmation)
	assert.False(t, byID[child.ID].Loadable, "metadata is not durable child access authorization")
	assert.Equal(t, "provider/model", byID[child.ID].Model)
	assert.False(t, byID[orphan.ID].RequiresConfirmation)
	assert.NotEmpty(t, byID[orphan.ID].RouteError)
	assert.EqualValues(t, 1, store.reads.Load())
	assert.Zero(t, registry.inspects, "browse never reads one tree per root")
	assert.Zero(t, registry.restores)
	assert.Zero(t, registry.createCount)
	roots, err := sm.ListSessionSummaries(t.Context(), runtime.SessionSummaryOptions{})
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, root.ID, roots[0].SessionID)
	local, err := sm.ListSessionSummaries(t.Context(), runtime.SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	// Use the real existing typed client, not a hand-constructed decoder.
	httpServer := httptest.NewServer(NewWithManager(sm, "").e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.ListSessionSummaries(t.Context(), runtime.SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	assert.Equal(t, local, remote)
	invalid := sessionRequest(t, srv, http.MethodGet, "/api/sessions?view=summary&include_children=invalid", "", "secret")
	assert.Equal(t, http.StatusBadRequest, invalid.Code)
}

func TestSessionSummaryCatalogUnavailableSourceExcluded(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root", sessionSourceAttribute: "missing-source"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker", sessionSourceAttribute: "missing-source"}))
	for _, sess := range []*session.Session{root, child} {
		require.NoError(t, base.AddSession(t.Context(), sess))
	}
	sm := NewSessionManager(t.Context(), nil, base, 0, nil, WithSessionRuntime(&httpSessionRegistry{sessions: map[string]*httpSession{}}))
	rows, err := sm.ListSessionSummaries(t.Context(), runtime.SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.False(t, row.Loadable)
		assert.False(t, row.RequiresConfirmation)
		assert.NotEmpty(t, row.RouteError)
	}
}
