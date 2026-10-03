package main

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
	client, err := newAuthorityClient(server.URL, "secret")
	require.NoError(t, err)
	data, err := client.request(t.Context(), http.MethodPost, "/s/messages", api.SessionInputRequest{Content: "hello", RequestID: "retry-key"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"session_id":"s","turn_id":"t"}`, string(data))
}

func TestAuthorityObservationChunksAndEpoch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "7", r.URL.Query().Get("since"))
		assert.Equal(t, "old", r.URL.Query().Get("since_epoch"))
		snap := authoritySnapshot{Session: session.New(session.WithID("s")), Status: api.SessionStatus[string]{SessionID: "s"}, Epoch: "new", Cursor: 2}
		data, err := json.Marshal(snap)
		assert.NoError(t, err)
		messages := []authorityMessage{{Version: 2, Type: "snapshot_begin", Cursor: 2}, {Version: 2, Type: "snapshot_chunk", Cursor: 2, Chunk: data}, {Version: 2, Type: "snapshot_end", Cursor: 2}, {Version: 2, Type: "ready", Cursor: 2}}
		for _, msg := range messages {
			data, err := json.Marshal(msg)
			assert.NoError(t, err)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer server.Close()
	client, err := newAuthorityClient(server.URL, "")
	require.NoError(t, err)
	since := uint64(7)
	var got []authorityMessage
	err = client.observe(t.Context(), "s", &since, "old", func(msg authorityMessage) bool { got = append(got, msg); return msg.Type != "ready" })
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "new", got[0].Snapshot.Epoch)
	assert.Equal(t, uint64(2), got[0].Snapshot.Cursor)
}

func TestAuthorityRejectsUnsafeURL(t *testing.T) {
	for _, base := range []string{"file:///tmp/socket", "http://user:pass@example.com", "https://example.com?token=secret", "//example.com"} {
		_, err := newAuthorityClient(base, "")
		require.Error(t, err)
	}
}

func TestAuthorityRejectsMalformedObservation(t *testing.T) {
	snapshot := authorityMessage{Version: 2, Type: "snapshot", Snapshot: &authoritySnapshot{Session: session.New(session.WithID("s")), Status: api.SessionStatus[string]{SessionID: "s"}, Epoch: "epoch", Cursor: 3}}
	envelope := func(id, epoch string, sequence uint64) authorityMessage {
		return authorityMessage{Version: 2, Type: "event", Envelope: &api.SessionEnvelope[json.RawMessage]{Version: 2, SessionID: id, Epoch: epoch, Sequence: sequence, Event: json.RawMessage(`{"type":"user_message"}`)}}
	}
	for name, messages := range map[string][]authorityMessage{
		"wrong version":           {{Version: 1, Type: "snapshot"}},
		"wrong snapshot identity": {{Version: 2, Type: "snapshot", Snapshot: &authoritySnapshot{Session: session.New(session.WithID("other")), Epoch: "epoch"}}},
		"chunk cursor mismatch":   {{Version: 2, Type: "snapshot_begin", Cursor: 3}, {Version: 2, Type: "snapshot_chunk", Cursor: 4, Chunk: []byte("{}")}},
		"chunk version mismatch":  {{Version: 2, Type: "snapshot_begin", Cursor: 3}, {Version: 1, Type: "snapshot_end", Cursor: 3}},
		"event before snapshot":   {envelope("s", "epoch", 4)},
		"wrong envelope identity": {snapshot, envelope("other", "epoch", 4)},
		"wrong envelope epoch":    {snapshot, envelope("s", "old", 4)},
		"duplicate sequence":      {snapshot, envelope("s", "epoch", 2), envelope("s", "epoch", 2)},
		"wrong ready cursor":      {snapshot, {Version: 2, Type: "ready", Cursor: 4}},
		"seed after ready":        {snapshot, {Version: 2, Type: "ready", Cursor: 3}, envelope("s", "epoch", 0)},
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
			client, err := newAuthorityClient(server.URL, "")
			require.NoError(t, err)
			err = client.observe(t.Context(), "s", nil, "", func(authorityMessage) bool { return true })
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
	client, err := newAuthorityClient(server.URL, "")
	require.NoError(t, err)
	var messages []authorityMessage
	require.NoError(t, client.observe(t.Context(), "s", nil, "", func(msg authorityMessage) bool { messages = append(messages, msg); return true }))
	require.Len(t, messages, 2)
	assert.True(t, messages[1].Envelope.Gap)
}
