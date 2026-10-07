package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
)

func TestRemotePendingRecallNegotiationAndExactOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supported bool
		result    CancelResult
		removed   bool
		errorKind SessionErrorKind
	}{
		{name: "removed", supported: true, result: CancelResult{SessionID: "child", TurnID: "queued", Outcome: CancelAccepted}, removed: true},
		{name: "promoted", supported: true, result: CancelResult{SessionID: "child", TurnID: "queued", Outcome: CancelNotActive}},
		{name: "permissive_old_peer", errorKind: SessionErrorUnsupported},
		{name: "wrong_session", supported: true, result: CancelResult{SessionID: "root", TurnID: "queued", Outcome: CancelAccepted}},
		{name: "wrong_turn", supported: true, result: CancelResult{SessionID: "child", TurnID: "active", Outcome: CancelAccepted}},
		{name: "active_cancellation_outcome", supported: true, result: CancelResult{SessionID: "child", TurnID: "queued", Outcome: CancelAlreadyCancelling}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					assert.Equal(t, api.SessionAPIPath+"/child/status", r.URL.Path)
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"metadata": api.SessionMetadata{SessionID: "child", Capabilities: api.SessionCapabilities{PendingMessageRemoval: tc.supported}}, "status": api.SessionStatus[SessionState]{SessionID: "child"}}))
					return
				}
				posts++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, api.SessionAPIPath+"/child/cancel", r.URL.Path)
				var request api.SessionCancelRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				assert.Equal(t, api.SessionCancelRequest{TurnID: "queued", PendingOnly: true}, request)
				// Deliberately permissive, as an older peer could silently ignore pending_only.
				require.NoError(t, json.NewEncoder(w).Encode(tc.result))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()), WithAuthToken("token"))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("child")
			require.NoError(t, err)
			canceler := handle.(PendingMessageCanceler)
			removed, err := canceler.CancelPendingMessage(t.Context(), "queued")
			assert.Equal(t, tc.removed, removed)
			if tc.errorKind != "" {
				var typed *SessionError
				require.ErrorAs(t, err, &typed)
				assert.Equal(t, tc.errorKind, typed.Kind)
				assert.Zero(t, posts, "unsupported peers must never receive a weaker cancel")
			} else if tc.name == "removed" || tc.name == "promoted" {
				require.NoError(t, err)
				assert.Equal(t, 1, posts)
			} else {
				require.Error(t, err)
			}
			removed, err = canceler.CancelPendingMessage(t.Context(), "")
			require.NoError(t, err)
			assert.False(t, removed)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			removed, err = canceler.CancelPendingMessage(ctx, "queued")
			require.ErrorIs(t, err, context.Canceled)
			assert.False(t, removed)
		})
	}
}
