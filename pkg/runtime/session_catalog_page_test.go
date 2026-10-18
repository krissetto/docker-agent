package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestLocalSessionSummaryPagesAreBoundedAndSourceScoped(t *testing.T) {
	store := session.NewInMemorySessionStore()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 7 {
		source := "one"
		if i%2 == 1 {
			source = "two"
		}
		sess := session.New(session.WithID(strconv.Itoa(i)), session.WithAgentName("root"), session.WithWorkingDir("/work"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root", "docker-agent.actor.source": source}))
		sess.CreatedAt = base.Add(time.Duration(i) * time.Second)
		require.NoError(t, store.AddSession(t.Context(), sess))
	}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/root"})))), WithSessionStore(store), WithSessionCompaction(false))
	require.NoError(t, err)
	defer rt.Close()
	pager := NewSessionRuntimeSupervisor(rt).Runtime().(SessionSummaryPager)
	first, err := pager.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{Source: "one", Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Entries, 1, "source filtering is post-page and remains bounded")
	assert.Equal(t, "6", first.Entries[0].SessionID)
	require.NotEmpty(t, first.NextCursor)
	second, err := pager.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{Source: "one", Limit: 2, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Entries, 1)
	assert.Equal(t, "4", second.Entries[0].SessionID)
	require.NotEmpty(t, second.NextCursor)
	_, err = pager.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{Source: "two", Limit: 2, Cursor: first.NextCursor})
	require.Error(t, err, "cursor cannot be replayed under another source")
}

func TestRemoteSessionSummaryPageMakesOneRequestAndScopesCursor(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "summary", r.URL.Query().Get("view"))
		assert.Equal(t, "true", r.URL.Query().Get("include_children"))
		assert.Equal(t, "2", r.URL.Query().Get("limit"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":2,"view":"summary","sessions":[{"session_id":"one","source":"team.yaml"}],"next_cursor":"Y3Vyc29yMiJ9"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
	require.NoError(t, err)
	page, err := transport.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{Source: "team.yaml", IncludeChildren: true, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, 1, calls, "one method call may perform only one bounded HTTP request")
	require.NotEmpty(t, page.NextCursor)
	_, err = transport.ListSessionSummaryPage(t.Context(), SessionSummaryPageOptions{Source: "other.yaml", IncludeChildren: true, Limit: 2, Cursor: page.NextCursor})
	require.Error(t, err, "cursor cannot cross source boundary")
	assert.Equal(t, 1, calls)
}

func TestRemoteSessionSummaryPageRejectsCrossSourceCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("must reject before network") }))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
	require.NoError(t, err)
	_, err = transport.ListSessionSummaryPage(context.Background(), SessionSummaryPageOptions{Source: "other.yaml", Limit: 20})
	require.Error(t, err)
}
