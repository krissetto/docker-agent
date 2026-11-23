package authority

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func TestAuthorityRequestUsesCanonicalTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, api.SessionAPIPath+"/s/messages", r.URL.Path)
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		var input api.SessionInputRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		assert.Equal(t, "retry-key", input.RequestID)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"session_id":"s","turn_id":"t"}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret")
	require.NoError(t, err)
	data, err := client.Request(t.Context(), http.MethodPost, "/s/messages", api.SessionInputRequest{Content: "hello", RequestID: "retry-key"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"session_id":"s","turn_id":"t"}`, string(data))
}

func TestAuthorityObservationChunksAndEpoch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "7", r.URL.Query().Get("since"))
		assert.Equal(t, "old", r.URL.Query().Get("since_epoch"))
		snap := snapshot{Session: session.New(session.WithID("s")), Status: api.SessionStatus[string]{SessionID: "s"}, Epoch: "new", Cursor: 2}
		data, err := json.Marshal(snap)
		assert.NoError(t, err)
		messages := []Message{{Version: 2, Type: "snapshot_begin", Cursor: 2}, {Version: 2, Type: "snapshot_chunk", Cursor: 2, Chunk: data}, {Version: 2, Type: "snapshot_end", Cursor: 2}, {Version: 2, Type: "ready", Cursor: 2}}
		for _, msg := range messages {
			data, err := json.Marshal(msg)
			assert.NoError(t, err)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "")
	require.NoError(t, err)
	since := uint64(7)
	var got []Message
	err = client.Observe(t.Context(), "s", &since, "old", func(msg Message) bool { got = append(got, msg); return msg.Type != "ready" })
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "new", got[0].Snapshot.Epoch)
	assert.Equal(t, uint64(2), got[0].Snapshot.Cursor)
}

func TestAuthorityRejectsUnsafeURL(t *testing.T) {
	for _, base := range []string{"file:///tmp/socket", "http://user:pass@example.com", "https://example.com?token=secret", "//example.com"} {
		_, err := NewClient(base, "")
		require.Error(t, err)
	}
}

func TestAuthorityRejectsMalformedObservation(t *testing.T) {
	baseline := Message{Version: 2, Type: "snapshot", Snapshot: &snapshot{Session: session.New(session.WithID("s")), Status: api.SessionStatus[string]{SessionID: "s"}, Epoch: "epoch", Cursor: 3}}
	envelope := func(id, epoch string, sequence uint64) Message {
		return Message{Version: 2, Type: "event", Envelope: &api.SessionEnvelope[json.RawMessage]{Version: 2, SessionID: id, Epoch: epoch, Sequence: sequence, Event: json.RawMessage(`{"type":"user_message"}`)}}
	}
	for name, messages := range map[string][]Message{
		"wrong version":             {{Version: 1, Type: "snapshot"}},
		"wrong snapshot identity":   {{Version: 2, Type: "snapshot", Snapshot: &snapshot{Session: session.New(session.WithID("other")), Epoch: "epoch"}}},
		"chunk cursor mismatch":     {{Version: 2, Type: "snapshot_begin", Cursor: 3}, {Version: 2, Type: "snapshot_chunk", Cursor: 4, Chunk: []byte("{}")}},
		"chunk version mismatch":    {{Version: 2, Type: "snapshot_begin", Cursor: 3}, {Version: 1, Type: "snapshot_end", Cursor: 3}},
		"event before snapshot":     {envelope("s", "epoch", 4)},
		"wrong envelope identity":   {baseline, envelope("other", "epoch", 4)},
		"wrong envelope epoch":      {baseline, envelope("s", "old", 4)},
		"baseline sequence hole":    {baseline, envelope("s", "epoch", 5)},
		"live sequence hole":        {baseline, {Version: 2, Type: "ready", Cursor: 3}, envelope("s", "epoch", 5)},
		"foreign event identity":    {baseline, {Version: 2, Type: "event", Envelope: &api.SessionEnvelope[json.RawMessage]{Version: 2, SessionID: "s", Epoch: "epoch", Sequence: 4, Event: json.RawMessage(`{"type":"agent_choice","session_id":"other","content":"foreign"}`)}}},
		"foreign interaction token": {baseline, {Version: 2, Type: "event", Envelope: &api.SessionEnvelope[json.RawMessage]{Version: 2, SessionID: "s", Epoch: "epoch", Sequence: 4, InteractionID: "token", Event: json.RawMessage(`{"type":"tool_call_confirmation","session_id":"s","request_id":"other"}`)}}},
		"duplicate sequence":        {baseline, envelope("s", "epoch", 2), envelope("s", "epoch", 2)},
		"wrong ready cursor":        {baseline, {Version: 2, Type: "ready", Cursor: 4}},
		"seed after ready":          {baseline, {Version: 2, Type: "ready", Cursor: 3}, envelope("s", "epoch", 0)},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, msg := range messages {
					data, err := json.Marshal(msg)
					assert.NoError(t, err)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "")
			require.NoError(t, err)
			err = client.Observe(t.Context(), "s", nil, "", func(Message) bool { return true })
			require.Error(t, err)
			assert.NotErrorIs(t, err, io.EOF)
		})
	}
}

func TestAuthorityGapIsTerminalBarrier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"epoch\":\"epoch\",\"cursor\":3}}\n\n")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"epoch\":\"epoch\",\"gap\":true}}\n\n")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":3}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "")
	require.NoError(t, err)
	var messages []Message
	require.NoError(t, client.Observe(t.Context(), "s", nil, "", func(msg Message) bool { messages = append(messages, msg); return true }))
	require.Len(t, messages, 2)
	assert.True(t, messages[1].Envelope.Gap)
}
