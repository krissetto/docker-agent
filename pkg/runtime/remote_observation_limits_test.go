package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoteObservationRejectsOversizedReplayAndHasNoOuterQueue(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(strconv.FormatBool(oversized), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"state\":\"settled\"},\"cursor\":0}}\n\n")
				if oversized {
					for i := range 10 {
						event, err := json.Marshal(UserMessage(strings.Repeat("x", 1<<20), "s", nil))
						if err != nil {
							return
						}
						fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"sequence\":%d,\"event\":%s}}\n\n", i+1, event)
					}
				}
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("s")
			require.NoError(t, err)
			observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 4 * maxSessionEventSubscriberBuffer})
			if oversized {
				require.ErrorContains(t, err, "observation limits")
				return
			}
			require.NoError(t, err)
			defer observation.Cancel()
			require.Zero(t, cap(observation.Events))
		})
	}
}
