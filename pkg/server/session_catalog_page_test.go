package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

type boundedCatalogStore struct {
	session.Store
	session.PagedSummaryStore

	limits []int
}

func (*boundedCatalogStore) GetSessions(context.Context) ([]*session.Session, error) {
	return nil, errors.New("catalog must not read transcripts")
}

func (*boundedCatalogStore) GetSession(context.Context, string) (*session.Session, error) {
	return nil, errors.New("catalog must not hydrate rows")
}

func (*boundedCatalogStore) GetSessionSummaries(context.Context) ([]session.Summary, error) {
	return nil, errors.New("catalog must use bounded pages")
}

func (s *boundedCatalogStore) GetSessionSummaryPage(ctx context.Context, options session.SummaryPageOptions) (session.SummaryPage, error) {
	s.limits = append(s.limits, options.Limit)
	return s.PagedSummaryStore.GetSessionSummaryPage(ctx, options)
}

func TestSessionCatalogBoundedMetadataCursorPages(t *testing.T) {
	base := session.NewInMemorySessionStore()
	for _, id := range []string{"c", "a", "b"} {
		row := session.New(session.WithID(id), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}), session.WithUserMessage("secret transcript"))
		row.CreatedAt = time.Unix(123, 0).UTC()
		require.NoError(t, base.AddSession(t.Context(), row))
	}
	store := &boundedCatalogStore{Store: base, PagedSummaryStore: base.(session.PagedSummaryStore)}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv := NewWithManager(NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(registry)), "")
	for _, view := range []string{"", "summary"} {
		var ids []string
		cursor := ""
		for {
			endpoint := api.SessionAPIPath + "?view=" + view + "&limit=1&cursor=" + url.QueryEscape(cursor)
			response := sessionRequest(t, srv, http.MethodGet, endpoint, "", "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), "secret transcript")
			assert.NotContains(t, response.Body.String(), `"messages"`)
			var page api.SessionSummaryCatalog[runtime.SessionSummaryEntry]
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
			require.Len(t, page.Sessions, 1)
			ids = append(ids, page.Sessions[0].SessionID)
			if page.NextCursor == "" {
				break
			}
			require.NotEqual(t, cursor, page.NextCursor)
			cursor = page.NextCursor
			wrongScope := sessionRequest(t, srv, http.MethodGet, api.SessionAPIPath+"?view="+view+"&include_children=true&cursor="+url.QueryEscape(cursor), "", "")
			assert.Equal(t, http.StatusBadRequest, wrongScope.Code)
		}
		assert.Equal(t, []string{"a", "b", "c"}, ids)
	}
	for _, limit := range store.limits {
		assert.Equal(t, 1, limit)
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=no", "cursor=invalid", "view=unknown"} {
		response := sessionRequest(t, srv, http.MethodGet, api.SessionAPIPath+"?"+query, "", "")
		assert.Equal(t, http.StatusBadRequest, response.Code)
	}
}

func TestRemoteSessionCatalogUsesPersistedItemCountAndSessionCost(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root := session.New(session.WithID("remote-root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}), session.WithUserMessage("question"))
	root.AddMessage(session.SystemMessage("system message is included in remote row count"))
	root.Cost = 2.5
	child := session.New(session.WithID("remote-child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}), session.WithUserMessage("child question"))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	server := httptest.NewServer(NewWithManager(NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(registry)), "").e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	rows, err := transport.ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, root.ID, rows[0].SessionID)
	assert.Equal(t, 2, rows[0].NumMessages, "remote resource catalog preserves raw message-item count, including system rows")
	assert.Equal(t, 2.5, rows[0].Cost, "remote resource catalog preserves the persisted sessions.cost field")
	children, err := transport.ListSessionSummaries(t.Context(), runtime.SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	require.Len(t, children, 2)
	byID := map[string]runtime.SessionSummaryEntry{}
	for _, row := range children {
		byID[row.SessionID] = row
	}
	assert.Equal(t, 2, byID[root.ID].NumMessages)
	assert.Equal(t, 1, byID[child.ID].NumMessages)
}
