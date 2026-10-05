package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoteSessionRejectsMalformedPeerEvents(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, tc := range []struct {
			name, event string
			sequence    uint64
		}{
			{"foreign session", `{"type":"agent_choice","session_id":"other","content":"foreign"}`, 2},
			{"foreign token", `{"type":"tool_call_confirmation","session_id":"s","request_id":"other"}`, 2},
			{"hole", `{"type":"agent_choice","session_id":"s","content":"hole"}`, 3},
			{"duplicate", `{"type":"agent_choice","session_id":"s","content":"duplicate"}`, 1},
		} {
			t.Run(fmt.Sprintf("%s/live=%t", tc.name, live), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"epoch\":\"e\",\"cursor\":1}}\n\n")
					if live {
						fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":1}\n\n")
					}
					fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"epoch\":\"e\",\"sequence\":%d,\"interaction_id\":\"token\",\"event\":%s}}\n\n", tc.sequence, tc.event)
				}))
				defer server.Close()
				client, err := NewClient(server.URL)
				require.NoError(t, err)
				since := uint64(1)
				obs, err := client.attachSession(t.Context(), "s", ObserveOptions{Since: &since, SinceEpoch: "e"})
				if live {
					require.NoError(t, err)
					defer obs.Cancel()
					_, open := <-obs.Events
					require.False(t, open, "malformed frame must not reach a consumer")
					err = <-obs.Errors
				}
				require.Error(t, err)
				var classified interface{ Retryable() bool }
				require.ErrorAs(t, err, &classified)
				require.False(t, classified.Retryable())
			})
		}
	}
}

func TestRemoteSessionTreeContinuityIsPerSession(t *testing.T) {
	for _, hole := range []bool{false, true} {
		t.Run(fmt.Sprintf("hole=%t", hole), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"root\"},\"status\":{\"session_id\":\"root\"},\"epoch\":\"e\",\"cursor\":5}}\n\n")
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"parent_session_id\":\"root\",\"session\":{\"id\":\"child\"},\"status\":{\"session_id\":\"child\"},\"epoch\":\"e\",\"cursor\":20}}\n\ndata: {\"version\":2,\"type\":\"ready\"}\n\n")
				for _, item := range []struct {
					id       string
					sequence uint64
				}{{"root", 6}, {"child", 21}, {"root", 7}, {"child", 22}} {
					if hole && item.sequence == 22 {
						item.sequence++
					}
					fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":%q,\"epoch\":\"e\",\"sequence\":%d,\"event\":{\"type\":\"agent_choice\",\"session_id\":%q,\"content\":\"tail\"}}}\n\n", item.id, item.sequence, item.id)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			obs, err := client.attachSession(t.Context(), "root", ObserveOptions{Tree: true, OrderedTree: true})
			require.NoError(t, err)
			defer obs.Cancel()
			for range 3 {
				_, open := <-obs.TreeUpdates
				require.True(t, open)
			}
			update, open := <-obs.TreeUpdates
			if hole {
				require.False(t, open)
				require.ErrorContains(t, <-obs.Errors, "sequence")
			} else {
				require.True(t, open)
				require.Equal(t, uint64(22), update.Event.Sequence)
			}
		})
	}
}

func TestRemoteSessionNestedInteractionIdentity(t *testing.T) {
	client, err := NewClient("http://example.invalid")
	require.NoError(t, err)
	for _, kind := range []string{"tool_call_confirmation", "max_iterations_reached", "elicitation_request", "interaction_resolved"} {
		t.Run(kind, func(t *testing.T) {
			for _, token := range []string{"token", "other"} {
				payload := fmt.Sprintf(`{"type":%q,"session_id":"s","request_id":%q,"interaction_id":%q,"reason":"responded"}`, kind, token, token)
				_, err := client.decodeSessionEnvelope(remoteSessionEnvelope{Version: 2, SessionID: "s", InteractionID: "token", Sequence: 1, Event: json.RawMessage(payload)})
				if token == "token" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
}

func TestRemoteSessionDynamicBaselineContinuity(t *testing.T) {
	for _, sequence := range []uint64{10, 11} {
		t.Run(strconv.FormatUint(sequence, 10), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"root\"},\"status\":{\"session_id\":\"root\"},\"epoch\":\"e\",\"cursor\":0}}\n\ndata: {\"version\":2,\"type\":\"ready\"}\n\n")
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"parent_session_id\":\"root\",\"session\":{\"id\":\"child\"},\"status\":{\"session_id\":\"child\"},\"epoch\":\"e\",\"cursor\":9,\"live_seeds\":[{\"version\":2,\"session_id\":\"child\",\"epoch\":\"e\",\"sequence\":0,\"transcript_position\":-1,\"event\":{\"type\":\"agent_choice\",\"session_id\":\"child\",\"content\":\"seed\"}}]}}\n\n")
				fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"child\",\"epoch\":\"e\",\"sequence\":%d,\"event\":{\"type\":\"agent_choice\",\"session_id\":\"child\",\"content\":\"tail\"}}}\n\n", sequence)
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			obs, err := client.attachSession(t.Context(), "root", ObserveOptions{Tree: true, OrderedTree: true})
			require.NoError(t, err)
			defer obs.Cancel()
			baseline, open := <-obs.TreeUpdates
			require.True(t, open)
			require.Equal(t, uint64(9), baseline.Snapshot.Cursor)
			require.Len(t, baseline.Replay, 1)
			require.True(t, baseline.Replay[0].IsLiveSeed())
			update, open := <-obs.TreeUpdates
			if sequence == 10 {
				require.True(t, open)
				require.Equal(t, sequence, update.Event.Sequence)
			} else {
				require.False(t, open)
				require.ErrorContains(t, <-obs.Errors, "sequence")
			}
		})
	}
}
