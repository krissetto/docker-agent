package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
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

func TestSessionSummarySearchPagesAndQueryCursorFence(t *testing.T) {
	base := session.NewInMemorySessionStore()
	for i, title := range []string{"unmatched", "TARGET title", "target second"} {
		row := session.New(session.WithID(title), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
		row.Title = title
		row.CreatedAt = time.Unix(100-int64(i), 0).UTC()
		require.NoError(t, base.AddSession(t.Context(), row))
	}
	store := &boundedCatalogStore{Store: base, PagedSummaryStore: base.(session.PagedSummaryStore)}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv := NewWithManager(NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(registry)), "")
	endpoint := api.SessionAPIPath + "?view=summary&limit=1&query=target"
	response := sessionRequest(t, srv, http.MethodGet, endpoint, "", "")
	require.Equal(t, http.StatusOK, response.Code)
	var page api.SessionSummaryCatalog[runtime.SessionSummaryEntry]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Equal(t, "target", page.Query)
	require.Len(t, page.Sessions, 1)
	require.Equal(t, "TARGET title", page.Sessions[0].Title)
	require.NotEmpty(t, page.NextCursor)
	require.Len(t, store.limits, 1)
	for _, scope := range []string{"", "&query=different", "&query=target&include_children=true"} {
		bad := sessionRequest(t, srv, http.MethodGet, api.SessionAPIPath+"?view=summary&cursor="+url.QueryEscape(page.NextCursor)+scope, "", "")
		require.Equal(t, http.StatusBadRequest, bad.Code)
	}
	response = sessionRequest(t, srv, http.MethodGet, endpoint+"&cursor="+url.QueryEscape(page.NextCursor), "", "")
	require.Equal(t, http.StatusOK, response.Code)
	page = api.SessionSummaryCatalog[runtime.SessionSummaryEntry]{}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Sessions, 1)
	require.Equal(t, "target second", page.Sessions[0].Title)
	require.Empty(t, page.NextCursor)
	require.Equal(t, []int{1, 1}, store.limits)
}
