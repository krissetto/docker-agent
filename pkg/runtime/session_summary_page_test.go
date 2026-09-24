package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

type boundedRuntimeSummaryStore struct {
	*metadataOnlyCatalogStore
	paged session.PagedSummaryStore
	calls []session.SummaryPageOptions
	err   error
}

func (s *boundedRuntimeSummaryStore) GetSessionSummaryPage(ctx context.Context, options session.SummaryPageOptions) (session.SummaryPage, error) {
	s.calls = append(s.calls, options)
	if s.err != nil {
		return session.SummaryPage{}, s.err
	}
	return s.paged.GetSessionSummaryPage(ctx, options)
}

func TestSessionSummaryPagerBoundedSearchAndCursorScope(t *testing.T) {
	base := session.NewInMemorySessionStore()
	for i, id := range []string{"unmatched", "root", "child"} {
		row := session.New(session.WithID(id), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
		row.CreatedAt = time.Unix(100-int64(i), 0).UTC()
		if i == 1 {
			row.Title = "Find title"
		}
		if i == 2 {
			row.ParentID = "root"
			row.WorkingDir = "/other/project/find"
		}
		require.NoError(t, base.AddSession(t.Context(), row))
	}
	store := &boundedRuntimeSummaryStore{metadataOnlyCatalogStore: &metadataOnlyCatalogStore{Store: base, ScopedSummaryStore: base.(session.ScopedSummaryStore)}, paged: base.(session.PagedSummaryStore)}
	_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	pager := owner.Runtime().(SessionSummaryPager)
	options := SessionSummaryPageOptions{Limit: 1, Query: " find ", IncludeChildren: true}
	page, err := pager.ListSessionSummaryPage(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.Equal(t, "root", page.Entries[0].SessionID)
	require.True(t, page.Entries[0].Loadable)
	require.NotEmpty(t, page.NextCursor)
	require.Len(t, store.calls, 1)
	require.Equal(t, "find", store.calls[0].Query)
	require.Equal(t, 1, store.calls[0].Limit)
	options.Cursor = page.NextCursor
	for _, invalid := range []SessionSummaryPageOptions{
		{Cursor: options.Cursor, Query: "other", IncludeChildren: true},
		{Cursor: options.Cursor, Query: "find"},
		{Cursor: "invalid*"}, {Limit: -1}, {Limit: 201},
	} {
		_, err := pager.ListSessionSummaryPage(t.Context(), invalid)
		require.ErrorIs(t, err, &SessionError{Kind: SessionErrorInvalid})
	}
	require.Len(t, store.calls, 1, "invalid requests must not read storage")
	page, err = pager.ListSessionSummaryPage(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.Equal(t, "child", page.Entries[0].SessionID)
	require.True(t, page.Entries[0].RequiresConfirmation, "off-page ancestor must not trigger transcript lookup")
	require.False(t, page.Entries[0].Loadable)
	require.Empty(t, page.NextCursor)
	require.Len(t, store.calls, 2)
	require.Zero(t, store.reads.Load(), "never invokes all-pages summaries")
	_, err = owner.Runtime().SessionByID("child")
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = pager.ListSessionSummaryPage(ctx, SessionSummaryPageOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, store.calls, 2)
	store.err = errors.New("metadata unavailable")
	_, err = pager.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{})
	require.ErrorIs(t, err, store.err)
	require.Equal(t, 50, store.calls[2].Limit)
}

func TestRemoteSessionSummaryPagerOneRequestAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		catalog     api.SessionSummaryCatalog[SessionSummaryEntry]
		wantError   bool
		unsupported bool
	}{
		{"page", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Query: "find", Sessions: []SessionSummaryEntry{{SessionID: "older", Title: "find"}}, NextCursor: "next"}, false, false},
		{"old ignores search", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Sessions: []SessionSummaryEntry{{SessionID: "unfiltered"}}}, true, true},
		{"wrong view", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, Query: "find"}, true, true},
		{"oversize", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Query: "find", Sessions: []SessionSummaryEntry{{SessionID: "a"}, {SessionID: "b"}}}, true, false},
		{"empty continuation", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Query: "find", NextCursor: "next"}, true, false},
		{"cursor unchanged", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Query: "find", Sessions: []SessionSummaryEntry{{SessionID: "a"}}, NextCursor: "previous"}, true, false},
		{"wrong scope", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Query: "find", Sessions: []SessionSummaryEntry{{SessionID: "a", ParentID: "root"}}}, true, false},
		{"missing identity", api.SessionSummaryCatalog[SessionSummaryEntry]{Version: sessionWireVersion, View: "summary", Query: "find", Sessions: []SessionSummaryEntry{{}}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				require.Equal(t, "find", r.URL.Query().Get("query"))
				require.Equal(t, "1", r.URL.Query().Get("limit"))
				require.Equal(t, "previous", r.URL.Query().Get("cursor"))
				require.Equal(t, "summary", r.URL.Query().Get("view"))
				require.NoError(t, json.NewEncoder(w).Encode(tc.catalog))
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			page, err := transport.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{Limit: 1, Query: " find ", Cursor: "previous"})
			if tc.wantError {
				require.Error(t, err)
				require.Empty(t, page)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.catalog.Sessions, page.Entries)
				require.Equal(t, "next", page.NextCursor)
			}
			if tc.unsupported {
				require.ErrorIs(t, err, ErrUnsupported)
			}
			require.Equal(t, 1, requests)
		})
	}
}

func TestSessionSummaryPagerLiveBindingAndUnsupportedStore(t *testing.T) {
	base := session.NewInMemorySessionStore()
	metadata := &metadataOnlyCatalogStore{Store: base, ScopedSummaryStore: base.(session.ScopedSummaryStore)}
	metadata.allowReads.Store(true)
	store := &boundedRuntimeSummaryStore{metadataOnlyCatalogStore: metadata, paged: base.(session.PagedSummaryStore)}
	_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	handle := coordinationCreate(t, owner.Runtime(), "live", "")
	handle.(*sessionHandle).driver.SetModelBinding("canonical/live-model", nil)
	metadata.allowReads.Store(false)
	page, err := owner.Runtime().(SessionSummaryPager).ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.True(t, page.Entries[0].Loaded)
	require.True(t, page.Entries[0].Loadable)
	require.Equal(t, "canonical/live-model", page.Entries[0].Model)
	require.Len(t, store.calls, 1)
	require.Equal(t, 50, store.calls[0].Limit)
	_, unsupportedOwner := coordinationRuntime(t, metadata, coordinationReply("root"), coordinationReply("child"))
	_, err = unsupportedOwner.Runtime().(SessionSummaryPager).ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{})
	require.ErrorIs(t, err, ErrUnsupported)
}
