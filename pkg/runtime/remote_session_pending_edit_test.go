package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRemotePendingEditExistingPatchAndTypedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, api.SessionAPIPath+"/child", r.URL.Path)
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		var edit SessionEdit
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&edit)) {
			http.Error(w, "invalid edit request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, pendingEdit("turn", "new", "old"), edit)
		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(t, json.NewEncoder(w).Encode(session.New(session.WithID("child")))) {
			http.Error(w, "encode session response", http.StatusInternalServerError)
			return
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()), WithAuthToken("token"))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("child")
	require.NoError(t, err)
	snapshot, err := handle.Edit(t.Context(), pendingEdit("turn", "new", "old"))
	require.NoError(t, err)
	assert.Equal(t, "child", snapshot.ID)
	event := PendingUserMessageEdited("child", "turn", "new", nil, 3)
	wire, err := json.Marshal(event)
	require.NoError(t, err)
	decoded := client.registry["pending_user_message_edited"]()
	require.NoError(t, json.Unmarshal(wire, decoded))
	typed := decoded.(*PendingUserMessageEditedEvent)
	assert.Equal(t, "new", typed.Message)
	assert.Equal(t, "turn", typed.TurnID)
	assert.Equal(t, 3, sessionEnvelope("child", SequencedSessionEvent{Event: decoded}).TranscriptPosition)
}

func TestRemotePendingEditTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   SessionErrorKind
	}{{http.StatusPreconditionFailed, SessionErrorStale}, {http.StatusNotImplemented, SessionErrorUnsupported}, {http.StatusServiceUnavailable, SessionErrorPersistence}, {http.StatusBadRequest, SessionErrorInvalid}} {
		t.Run(string(tc.kind), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if !assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": tc.kind, "session_id": "s", "operation": "edit_pending_message", "detail": "pending message no longer editable"})) {
					http.Error(w, "encode error response", http.StatusInternalServerError)
					return
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("s")
			require.NoError(t, err)
			_, err = handle.Edit(t.Context(), pendingEdit("turn", "new", "old"))
			var typed *SessionError
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, tc.kind, typed.Kind)
			assert.Equal(t, SessionOperation("edit_pending_message"), typed.Operation)
		})
	}
}
