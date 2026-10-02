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
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRemoteLateAttachLiveSeeds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*remoteSessionEnvelope)
		since     *uint64
		live      bool
		wantError bool
	}{
		{name: "current_output"},
		{name: "explicit_cursor", since: new(uint64), wantError: true},
		{name: "live_tail", live: true, wantError: true},
		{name: "arbitrary_event", mutate: func(e *remoteSessionEnvelope) {
			e.Event = json.RawMessage(`{"type":"session_title","session_id":"s","title":"not a seed"}`)
		}, wantError: true},
		{name: "empty_output", mutate: func(e *remoteSessionEnvelope) {
			e.Event = json.RawMessage(`{"type":"agent_choice","session_id":"s","content":""}`)
		}, wantError: true},
		{name: "wrong_session", mutate: func(e *remoteSessionEnvelope) { e.SessionID = "other" }, wantError: true},
		{name: "wrong_event_session", mutate: func(e *remoteSessionEnvelope) {
			e.Event = json.RawMessage(`{"type":"stream_started","session_id":"other"}`)
		}, wantError: true},
		{name: "correlated_turn", mutate: func(e *remoteSessionEnvelope) { e.TurnID = "turn" }, wantError: true},
		{name: "correlated_interaction", mutate: func(e *remoteSessionEnvelope) { e.InteractionID = "interaction" }, wantError: true},
		{name: "transcript_position", mutate: func(e *remoteSessionEnvelope) { e.TranscriptPosition = 0 }, wantError: true},
		{name: "first_available", mutate: func(e *remoteSessionEnvelope) { e.FirstAvailable = 4 }, wantError: true},
		{name: "real_sequence_not_newer", mutate: func(e *remoteSessionEnvelope) { e.Sequence = 8 }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, api.SessionAPIPath+"/s/events", r.URL.Path)
				w.Header().Set("Content-Type", "text/event-stream")
				write := func(m remoteSessionStreamMessage) {
					data, err := json.Marshal(m)
					if !assert.NoError(t, err) { //nolint:testifylint // HTTP handlers cannot use require, which calls FailNow outside the test goroutine.
						return
					}
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				snapshot := remoteSessionSnapshot{Session: session.New(session.WithID("s")), Status: api.SessionStatus[SessionState]{SessionID: "s", State: SessionStateRunning}, Cursor: 8}
				write(remoteSessionStreamMessage{Version: api.SessionAPIVersion, Type: "snapshot", Snapshot: &snapshot})
				ready := remoteSessionStreamMessage{Version: api.SessionAPIVersion, Type: "ready", Cursor: 8}
				if tc.live {
					write(ready)
				}
				for _, event := range []Event{StreamStarted("s", "root"), AgentChoiceReasoning("root", "s", "thinking"), AgentChoice("root", "s", "partial output"), PartialToolCall(tools.ToolCall{ID: "call"}, tools.Tool{}, "root"), ToolCall(tools.ToolCall{ID: "call"}, tools.Tool{}, "root"), ToolCallOutput("call", tools.Tool{}, "output", "root")} {
					raw, err := json.Marshal(event)
					if !assert.NoError(t, err) {
						return
					}
					envelope := remoteSessionEnvelope{Version: api.SessionAPIVersion, SessionID: "s", TranscriptPosition: -1, Event: raw}
					if tc.mutate != nil {
						tc.mutate(&envelope)
					}
					write(remoteSessionStreamMessage{Version: api.SessionAPIVersion, Type: "event", Envelope: &envelope})
				}
				if !tc.live {
					write(ready)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("s")
			require.NoError(t, err)
			obs, err := handle.Observe(t.Context(), ObserveOptions{Since: tc.since})
			if tc.wantError && !tc.live {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			defer obs.Cancel()
			if tc.live {
				require.ErrorContains(t, <-obs.Errors, "invalid session observation sequence")
				return
			}
			require.Len(t, obs.Replay, 6)
			for _, seed := range obs.Replay {
				assert.Zero(t, seed.Sequence)
			}
			assert.Equal(t, uint64(8), obs.Primary().Cursor)
			require.ErrorContains(t, <-obs.Errors, "terminated at cursor 8")
		})
	}
}
