package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRemoteTreeRejectsInvalidDynamicBaseline(t *testing.T) {
	for _, test := range []struct{ name, frame string }{
		{"wrong_version", `{"version":99,"type":"snapshot","snapshot":{"session":{"id":"child"},"status":{"session_id":"child"}}}`},
		{"duplicate_session", `{"version":2,"type":"snapshot","snapshot":{"session":{"id":"root"},"status":{"session_id":"root"}}}`},
		{"wrong_seed_session", `{"version":2,"type":"snapshot","snapshot":{"session":{"id":"child"},"status":{"session_id":"child"},"live_seeds":[{"version":2,"session_id":"other","sequence":0,"transcript_position":-1,"event":{"type":"agent_choice","session_id":"other","content":"seed"}}]}}`},
		{"journal_as_seed", `{"version":2,"type":"snapshot","snapshot":{"session":{"id":"child"},"status":{"session_id":"child"},"live_seeds":[{"version":2,"session_id":"child","sequence":1,"transcript_position":-1,"event":{"type":"agent_choice","session_id":"child","content":"seed"}}]}}`},
		{"unadmitted_live_session", `{"version":2,"type":"event","envelope":{"version":2,"session_id":"child","sequence":1,"event":{"type":"agent_choice","session_id":"child","content":"tail"}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"root\"},\"status\":{\"session_id\":\"root\"}}}\n\ndata: {\"version\":2,\"type\":\"ready\"}\n\n")
				fmt.Fprintf(w, "data: %s\n\n", test.frame)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("root")
			require.NoError(t, err)
			observation, err := handle.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: true})
			require.NoError(t, err)
			defer observation.Cancel()
			select {
			case err := <-observation.Errors:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("invalid dynamic baseline was not rejected")
			}
			_, open := <-observation.TreeUpdates
			require.False(t, open, "invalid baseline must not be admitted")
		})
	}
}
