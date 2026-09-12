package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteSessionCapabilityOperations(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/sessions/s/models":
			_, _ = w.Write([]byte(`[{"name":"fast","ref":"test/fast"}]`))
		case "/api/sessions/s/model":
			_, _ = w.Write([]byte(`{"session_id":"s","agent_name":"root","model":"test/fast","capabilities":{"model_switching":true,"model_catalog_refresh":true,"pause":true,"session_editing":true,"context_inspection":true,"live_sessions":true,"compaction":true,"target_compaction":true}}`))
		case "/api/sessions/s/models/refresh":
			_, _ = w.Write([]byte(`{"session_id":"s","agent_name":"root","model":"test/fast","capabilities":{"model_switching":true,"model_catalog_refresh":true,"pause":true,"session_editing":true,"context_inspection":true,"live_sessions":true,"compaction":true,"target_compaction":true}}`))
		case "/api/sessions/s/pause":
			_, _ = w.Write([]byte(`{"paused":true}`))
		case "/api/sessions/s/context":
			_, _ = w.Write([]byte(`{"model":"test/fast"}`))
		case "/api/sessions/s/live-sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s","agent_name":"root","current":true}]`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL)
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	session := &remoteSession{runtime: transport, sessionID: "s", metadata: SessionMetadata{SessionID: "s", AgentName: "root", Capabilities: SessionCapabilities{ModelSwitching: true, ModelCatalogRefresh: true, Pause: true, SessionEditing: true, ContextInspection: true, LiveSessions: true, Compaction: true, TargetCompaction: true}}}

	assert.Len(t, session.AvailableModels(t.Context()), 1)
	require.NoError(t, session.SetModel(t.Context(), "test/fast"))
	assert.Equal(t, "test/fast", session.Metadata().Model)
	require.NoError(t, session.RefreshModelsCatalog(t.Context()))
	paused, err := session.TogglePause(t.Context())
	require.NoError(t, err)
	assert.True(t, paused)
	_, err = session.ContextBreakdown(t.Context())
	require.NoError(t, err)
	_, err = session.LiveSessions(t.Context())
	require.NoError(t, err)
	require.NoError(t, session.SetStarred(t.Context(), true))
	require.NoError(t, session.RemoveAttachment(t.Context(), "/tmp/a"))
	require.NoError(t, session.Compact(t.Context(), "", EventSinkFunc(func(Event) {})))
	require.NoError(t, session.CompactTarget(t.Context(), "child", "", EventSinkFunc(func(Event) {})))
	assert.Contains(t, requests, "PATCH /api/sessions/s/model")
	assert.Contains(t, requests, "POST /api/sessions/s/compact/child")
}
