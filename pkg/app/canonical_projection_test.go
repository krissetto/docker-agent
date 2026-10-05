package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
)

func writeProjectionFrame(t *testing.T, w http.ResponseWriter, event runtime.SessionEvent) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"version": 2, "type": "event", "envelope": map[string]any{"version": 2, "session_id": event.SessionID, "turn_id": event.TurnID, "sequence": event.Sequence, "transcript_position": -1, "event": event.Event}})
	require.NoError(t, err)
	fmt.Fprintf(w, "data: %s\n\n", data)
}
func TestCanonicalProjectionHoldsBusyUntilSettlementLocalAndRemote(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			a := newMetadataTestApp(t)
			sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"}
			snapshot := runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateRunning, TurnID: "one"}, PendingInputs: []runtime.PendingInput{{TurnID: "two", Content: "next"}}}
			events := []runtime.SessionEvent{
				{SessionID: "s", TurnID: "one", Sequence: 1, Event: runtime.StreamStarted("s", "root")},
				{SessionID: "s", TurnID: "one", Sequence: 2, Event: runtime.StreamStopped("s", "root", "normal")},
				{SessionID: "s", TurnID: "one", Sequence: 3, Event: &runtime.TurnSettledEvent{Type: "turn_settled", SessionID: "s", TurnID: "one", Outcome: runtime.TurnCompleted}},
				{SessionID: "s", TurnID: "two", Sequence: 4, Event: &runtime.PendingUserMessagePromotedEvent{Type: "pending_user_message_promoted", SessionID: "s", TurnID: "two"}},
				{SessionID: "s", TurnID: "two", Sequence: 5, Event: runtime.StreamStarted("s", "root")},
			}
			var tail <-chan runtime.SessionEvent
			release := make(chan struct{})
			if remote {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != api.SessionAPIPath+"/s/events" {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, `data: {"version":2,"type":"snapshot","snapshot":{"session":{"id":"s"},"status":{"session_id":"s","state":"running","turn_id":"one"},"pending_inputs":[{"turn_id":"two","content":"next"}],"cursor":0,"transcript_position":0}}`+"\n\n")
					fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
					w.(http.Flusher).Flush()
					for i, event := range events {
						if i == 2 {
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
						}
						writeProjectionFrame(t, w, event)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				}))
				defer server.Close()
				client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
				require.NoError(t, err)
				transport, err := runtime.NewSessionTransport(client)
				require.NoError(t, err)
				handle, err := transport.SessionByID("s")
				require.NoError(t, err)
				observation, err := handle.Observe(t.Context(), runtime.ObserveOptions{})
				require.NoError(t, err)
				defer observation.Cancel()
				snapshot = observation.Primary()
				tail = observation.Events
			} else {
				ch := make(chan runtime.SessionEvent, len(events))
				for _, event := range events {
					ch <- event
				}
				tail = ch
			}
			sink.Reset(snapshot)
			for range 2 {
				sink.Apply(<-tail)
			}
			require.Zero(t, a.Presentation().Lifecycle.Depth())
			require.Equal(t, runtime.SessionStateRunning, a.Presentation().Status.State, "presentation EOF cannot release ownership")
			require.Equal(t, "one", a.Presentation().Status.TurnID)
			require.Len(t, a.Presentation().PendingInputs, 1)
			close(release)
			sink.Apply(<-tail)
			require.Equal(t, runtime.SessionStateSettled, a.Presentation().Status.State)
			require.Empty(t, a.Presentation().Status.TurnID)
			sink.Apply(<-tail)
			require.Empty(t, a.Presentation().PendingInputs)
			sink.Apply(<-tail)
			require.Equal(t, "two", a.Presentation().Status.TurnID)
			require.Equal(t, 1, a.Presentation().Lifecycle.Depth(), "successor starts exactly once")
		})
	}
}
