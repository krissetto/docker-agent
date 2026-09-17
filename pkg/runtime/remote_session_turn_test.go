package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteAwaitTurnExactIdentityAndContext(t *testing.T) {
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		switch r.URL.Path {
		case "/api/sessions/s/turns/settled/wait":
			w.WriteHeader(http.StatusNoContent)
		case "/api/sessions/s/turns/missing/wait", "/api/sessions/s/turns/expired/wait":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not_found","session_id":"s","operation":"await_turn"}`)
		case "/api/sessions/s/turns/active/wait":
			entered <- struct{}{}
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), "settled"))
	var sessionErr *SessionError
	require.ErrorAs(t, handle.AwaitTurn(t.Context(), "missing"), &sessionErr)
	assert.Equal(t, SessionErrorNotFound, sessionErr.Kind)
	require.ErrorAs(t, handle.AwaitTurn(t.Context(), "expired"), &sessionErr)
	assert.Equal(t, SessionErrorNotFound, sessionErr.Kind)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- handle.AwaitTurn(ctx, "active") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not reach server")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func snapshotFrames(t *testing.T, payload []byte, cursor uint64) []remoteSessionStreamMessage {
	t.Helper()
	frames := []remoteSessionStreamMessage{{Version: 1, Type: "snapshot_begin", Cursor: cursor}}
	for len(payload) > 0 {
		n := min(len(payload), remoteSnapshotChunkBytes)
		frames = append(frames, remoteSessionStreamMessage{Version: 1, Type: "snapshot_chunk", Cursor: cursor, Chunk: bytes.Clone(payload[:n])})
		payload = payload[n:]
	}
	return append(frames, remoteSessionStreamMessage{Version: 1, Type: "snapshot_end", Cursor: cursor})
}

func scanSnapshotFrames(t *testing.T, frames []remoteSessionStreamMessage) (remoteSessionStreamMessage, error) {
	t.Helper()
	var wire bytes.Buffer
	for _, frame := range frames {
		data, err := json.Marshal(frame)
		require.NoError(t, err)
		fmt.Fprintf(&wire, "data: %s\n\n", data)
	}
	scanner := bufio.NewScanner(&wire)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)
	return scanSessionMessage(scanner)
}

func TestRemoteChunkedSnapshotPreservesHistoryBeyondSSELimit(t *testing.T) {
	// A single transcript field can exceed the old 16MiB line ceiling.
	title := strings.Repeat("history", (maxSSELineBytes/7)+1)
	payload, err := json.Marshal(map[string]any{"session": map[string]any{"id": "s", "title": title}, "status": map[string]any{"session_id": "s"}, "cursor": 7})
	require.NoError(t, err)
	message, err := scanSnapshotFrames(t, snapshotFrames(t, payload, 7))
	require.NoError(t, err)
	require.NotNil(t, message.Snapshot)
	assert.Equal(t, title, message.Snapshot.Session.Title)
	assert.Equal(t, uint64(7), message.Snapshot.Cursor)
}

func TestRemoteChunkedSnapshotRejectsInvalidFraming(t *testing.T) {
	payload := []byte(`{"session":{"id":"s"},"status":{"session_id":"s"},"cursor":7}`)
	for _, tc := range []struct {
		name   string
		change func([]remoteSessionStreamMessage) []remoteSessionStreamMessage
	}{
		{"missing_end", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage { return f[:len(f)-1] }},
		{"changed_cursor", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage { f[1].Cursor++; return f }},
		{"changed_version", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage { f[1].Version++; return f }},
		{"oversized_chunk", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage {
			f[1].Chunk = bytes.Repeat([]byte(" "), remoteSnapshotChunkBytes+1)
			return f
		}},
		{"empty_chunk", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage { f[1].Chunk = nil; return f }},
		{"wrong_frame", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage { f[1].Type = "ready"; return f }},
		{"trailing_json", func(f []remoteSessionStreamMessage) []remoteSessionStreamMessage {
			f[1].Chunk = append(f[1].Chunk, []byte(` {}`)...)
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := scanSnapshotFrames(t, tc.change(snapshotFrames(t, payload, 7)))
			require.Error(t, err)
		})
	}
}

func TestRemoteInteractionResolutionIdentity(t *testing.T) {
	client, err := NewClient("http://localhost")
	require.NoError(t, err)
	for _, reason := range []string{"responded", "canceled", "stopped"} {
		payload := json.RawMessage(fmt.Sprintf(`{"type":"interaction_resolved","session_id":"s","interaction_id":"i","reason":%q}`, reason))
		envelope := remoteSessionEnvelope{Version: 1, SessionID: "s", InteractionID: "i", Sequence: 1, Event: payload}
		decoded, err := client.decodeSessionEnvelope(envelope)
		require.NoError(t, err)
		resolved, ok := decoded.Event.(*InteractionResolvedEvent)
		require.True(t, ok)
		assert.Equal(t, reason, string(resolved.Reason))
		envelope.InteractionID = "other"
		_, err = client.decodeSessionEnvelope(envelope)
		require.Error(t, err)
		envelope.InteractionID = ""
		_, err = client.decodeSessionEnvelope(envelope)
		require.Error(t, err)
	}
}

func TestRemoteChunkedSnapshotCancellationClosesTransport(t *testing.T) {
	entered, disconnected := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"version\":1,\"type\":\"snapshot_begin\",\"cursor\":1}\n\n")
		chunk, err := json.Marshal(remoteSessionStreamMessage{Version: 1, Type: "snapshot_chunk", Cursor: 1, Chunk: []byte(`{"session":{"id":"s","title":"`)})
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(disconnected)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := handle.Observe(ctx, ObserveOptions{}); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not begin")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot decode ignored cancellation")
	}
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot cancellation leaked HTTP connection")
	}
}

func TestRemoteCanonicalEventsMatchLocalEnvelope(t *testing.T) {
	client, err := NewClient("http://localhost")
	require.NoError(t, err)
	for _, event := range []Event{
		&TurnSettledEvent{Type: "turn_settled", SessionID: "owner", TurnID: "accepted", Outcome: TurnCompleted},
		&TurnSettledEvent{Type: "turn_settled", SessionID: "owner", TurnID: "accepted", Outcome: TurnCanceled},
		&TurnSettledEvent{Type: "turn_settled", SessionID: "owner", TurnID: "accepted", Outcome: TurnFailed},
		&SubagentCreatedEvent{Type: "subagent_created", SessionID: "owner", ParentSessionID: "owner", ChildSessionID: "child", NodeID: "node", CreatedAt: time.Unix(123, 0).UTC()},
	} {
		local := sessionEnvelope("owner", SequencedSessionEvent{Sequence: 42, RequestID: "accepted", Event: event})
		payload, err := json.Marshal(event)
		require.NoError(t, err)
		remote, err := client.decodeSessionEnvelope(remoteSessionEnvelope{
			Version: local.Version, SessionID: local.SessionID, TurnID: local.TurnID,
			Sequence: local.Sequence, TranscriptPosition: local.TranscriptPosition, Event: payload,
		})
		require.NoError(t, err)
		assert.Equal(t, local, remote, "typed remote transport must preserve the canonical event and envelope")
	}
}
