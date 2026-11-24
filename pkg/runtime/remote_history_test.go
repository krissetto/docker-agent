package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRemoteSelectedHistoryProjectionsAndLegacyFallback(t *testing.T) {
	for _, mode := range []string{"projection", "legacy_rejects", "legacy_ignores"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				projection := r.URL.Query().Get("projection")
				requests <- r.URL.Path + ":" + projection
				if projection != "" && mode == "legacy_rejects" {
					http.Error(w, "unsupported projection", http.StatusBadRequest)
					return
				}
				if mode == "legacy_ignores" {
					projection = ""
				}
				if r.URL.Path == api.SessionAPIPath+"/s/history" {
					assert.Equal(t, "older", r.URL.Query().Get("cursor"))
					_ = json.NewEncoder(w).Encode(api.SessionHistoryPage{
						Projection: "tail", SessionID: "s", Epoch: "epoch", Cursor: 4, TranscriptPosition: 2,
						History: api.SessionHistory{StartPosition: 0, EndPosition: 1, TotalItems: 2}, Items: []api.SessionHistoryItem{{Position: 0, Item: session.Item{Message: session.UserMessage("earlier")}}},
					})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				snapshot := remoteSessionSnapshot{Projection: projection, Session: session.New(session.WithID("s")), Status: api.SessionStatus[SessionState]{SessionID: "s", State: SessionStateSettled}, Epoch: "epoch", Cursor: 4, TranscriptPosition: 2}
				switch projection {
				case "tail":
					assert.Equal(t, "1", r.URL.Query().Get("max_items"))
					snapshot.Items = []api.SessionHistoryItem{{Position: 1, Item: session.Item{Message: session.UserMessage("recent")}}}
					snapshot.History = &api.SessionHistory{StartPosition: 1, EndPosition: 2, TotalItems: 2, NextCursor: "older", Truncated: true}
				case "":
					snapshot.Session.AddMessage(session.UserMessage("earlier"))
					snapshot.Session.AddMessage(session.UserMessage("recent"))
				}
				for _, frame := range []remoteSessionStreamMessage{{Version: sessionWireVersion, Type: "snapshot", Snapshot: &snapshot}, {Version: sessionWireVersion, Type: "ready", Cursor: 4}} {
					data, _ := json.Marshal(frame)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.Type, data)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client, WithSelectedSessionHistory())
			require.NoError(t, err)
			handle := &remoteSession{runtime: transport, sessionID: "s"}
			observation, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &SessionHistoryLimits{MaxItems: 1}})
			require.NoError(t, err)
			defer observation.Cancel()
			require.Equal(t, api.SessionAPIPath+"/s/events:tail", <-requests)
			if mode == "projection" {
				got := observation.Primary()
				require.Zero(t, got.Session.ItemCount())
				require.Equal(t, "older", got.HistoryCursor)
				require.Equal(t, 1, got.Items[0].Position)
				older, err := transport.ReadOlderHistory(t.Context(), "s", got.HistoryCursor, SessionHistoryLimits{MaxItems: 1})
				require.NoError(t, err)
				require.Equal(t, 1, older.History.BeforePosition)
				require.Equal(t, got.Cursor, older.Cursor)
				require.Equal(t, api.SessionAPIPath+"/s/history:tail", <-requests)
			} else {
				require.Nil(t, observation.Primary().History)
				require.Equal(t, 2, observation.Primary().Session.ItemCount())
				if mode == "legacy_rejects" {
					require.Equal(t, api.SessionAPIPath+"/s/events:", <-requests)
				}
			}
			tree, err := handle.ObserveTreeMetadata(t.Context())
			require.NoError(t, err)
			defer tree.Cancel()
			require.Equal(t, api.SessionAPIPath+"/s/events:metadata", <-requests)
			if mode == "projection" {
				require.Equal(t, "metadata", tree.Primary().Projection)
				require.Zero(t, tree.Primary().Session.ItemCount())
			} else {
				require.Equal(t, 2, tree.Primary().Session.ItemCount())
				if mode == "legacy_rejects" {
					require.Equal(t, api.SessionAPIPath+"/s/events:", <-requests)
				}
			}
			require.Empty(t, requests)
		})
	}
}
