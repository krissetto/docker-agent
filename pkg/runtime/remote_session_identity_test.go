package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteSessionRejectsOtherSessionBaseline(t *testing.T) {
	for _, operation := range []string{"snapshot", "observe", "tree"} {
		t.Run(operation, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				baseline := `{"session":{"id":"other"},"status":{"session_id":"other"},"epoch":"peer","cursor":1}`
				if operation == "snapshot" {
					fmt.Fprint(w, baseline)
				} else {
					fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":%s}\n\ndata: {\"version\":2,\"type\":\"ready\",\"cursor\":1}\n\n", baseline)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("requested")
			require.NoError(t, err)
			if operation == "snapshot" {
				snapshot, snapshotErr := handle.Snapshot(t.Context())
				require.Nil(t, snapshot)
				err = snapshotErr
			} else {
				observation, observeErr := handle.Observe(t.Context(), ObserveOptions{Tree: operation == "tree"})
				require.Empty(t, observation.Initial)
				err = observeErr
			}
			require.Error(t, err)
			var classified interface{ Retryable() bool }
			require.ErrorAs(t, err, &classified)
			require.False(t, classified.Retryable())
		})
	}
}

func TestRemoteSessionValidatesInitialTreeMembership(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members string
		valid   bool
	}{
		{"duplicate", `{"id":"root","parent_id":"root"}`, false},
		{"orphan", `{"id":"other"}`, false},
		{"cycle", `{"id":"child","parent_id":"child"}`, false},
		{"unordered descendants", `{"id":"grandchild","parent_id":"child"}|{"id":"child","parent_id":"root"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"root\"},\"status\":{\"session_id\":\"root\"}}}\n\n")
				for member := range strings.SplitSeq(tc.members, "|") {
					var identity struct {
						ID string `json:"id"`
					}
					if !assert.NoError(t, json.Unmarshal([]byte(member), &identity)) {
						return
					}
					var ancestry struct {
						Parent string `json:"parent_id"`
					}
					if !assert.NoError(t, json.Unmarshal([]byte(member), &ancestry)) {
						return
					}
					fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"parent_session_id\":%q,\"session\":%s,\"status\":{\"session_id\":%q}}}\n\n", ancestry.Parent, member, identity.ID)
				}
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			observation, err := client.attachSession(t.Context(), "root", ObserveOptions{Tree: true})
			if tc.valid {
				require.NoError(t, err)
				defer observation.Cancel()
				require.Len(t, observation.Initial, 3)
			} else {
				require.Error(t, err)
				require.Empty(t, observation.Initial)
			}
		})
	}
}

func TestRemoteSessionValidatesDynamicTreeMembership(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(strconv.FormatBool(valid), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"root\"},\"status\":{\"session_id\":\"root\"}}}\n\ndata: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
				parent := "root"
				if !valid {
					parent = "unrelated"
				}
				fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"parent_session_id\":%q,\"session\":{\"id\":\"child\"},\"status\":{\"session_id\":\"child\"}}}\n\n", parent)
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"parent_session_id\":\"child\",\"session\":{\"id\":\"grandchild\"},\"status\":{\"session_id\":\"grandchild\"}}}\n\n")
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			observation, err := client.attachSession(t.Context(), "root", ObserveOptions{Tree: true, OrderedTree: true})
			require.NoError(t, err)
			defer observation.Cancel()
			if valid {
				for _, id := range []string{"child", "grandchild"} {
					update, ok := <-observation.TreeUpdates
					require.True(t, ok)
					require.Equal(t, id, update.Snapshot.Session.ID)
				}
			} else {
				_, ok := <-observation.TreeUpdates
				require.False(t, ok, "unrelated baseline must never be projected")
				require.Error(t, <-observation.Errors)
			}
		})
	}
}

func TestRemoteSessionLegacyTreeRequiresCompleteWitness(t *testing.T) {
	for _, tc := range []struct {
		name, witness string
		valid         bool
	}{
		{"valid", `{"root":"root:root","nodes":[{"node":{"id":"root:root","session_id":"root"},"children":[{"node":{"id":"child-node","session_id":"child"}}]}]}`, true},
		{"wrong root", `{"root":"root:other","nodes":[{"node":{"id":"root:other","session_id":"other"}}]}`, false},
		{"inconsistent root", `{"root":"root:root","nodes":[{"node":{"id":"root:root","session_id":"other"}}]}`, false},
		{"unrelated child", `{"root":"root:root","nodes":[{"node":{"id":"root:root","session_id":"root"}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"root\",\"subagent_tree\":%s},\"status\":{\"session_id\":\"root\"}}}\n\n", tc.witness)
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"child\"},\"status\":{\"session_id\":\"child\"}}}\n\ndata: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			observation, err := client.attachSession(t.Context(), "root", ObserveOptions{Tree: true})
			if tc.valid {
				require.NoError(t, err)
				defer observation.Cancel()
				require.Equal(t, "root", observation.Initial[1].Session.ParentID)
			} else {
				require.Error(t, err)
				require.Empty(t, observation.Initial)
			}
		})
	}
}
