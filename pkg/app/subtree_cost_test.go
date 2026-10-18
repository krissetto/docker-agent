package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func ownCostSnapshot(id, parent string, cost float64) runtime.SessionSnapshot {
	sess := session.New(session.WithID(id), session.WithParentID(parent))
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Cost: cost}})
	return runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: id}}
}

func observedCost(t *testing.T, watch *treeWatch, base *subagent.Snapshot, id string) float64 {
	t.Helper()
	snapshot := watch.merged(base)
	require.NotNil(t, snapshot)
	node, found := findNode(snapshot.Nodes, func(n subagent.Node) bool { return n.SessionID == id || n.ID == subagent.SessionRootID(id) })
	require.True(t, found)
	return node.Cost
}

func TestTreeWatchOwnCostBaselineLiveReplayRestore(t *testing.T) {
	t.Parallel()
	base := &subagent.Snapshot{Root: "root:root", Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: "root:root", Agent: "root", Cost: 1},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: "child", Cost: 2, State: subagent.NodeCompleted},
			Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grand-node", SessionID: "grand", Cost: 3, State: subagent.NodeStopped}}}}},
	}}}
	root, child, grand := ownCostSnapshot("root", "", 1), ownCostSnapshot("child", "root", 2), ownCostSnapshot("grand", "child", 3)
	root.Session.SetSubagentTree(base)
	child.Session.AddSubSession(grand.Session)
	root.Session.AddSubSession(child.Session)
	watch := newTreeWatch("root")
	watch.reset([]runtime.SessionSnapshot{root, child, grand})
	assert.Equal(t, 1.0, observedCost(t, watch, base, "root"), "snapshot aggregate is never used as own cost")
	for i, cost := range []float64{5, 5, 4} {
		changed := watch.apply(runtime.SessionEvent{SessionID: "grand", Event: &runtime.TokenUsageEvent{SessionID: "grand", Usage: &runtime.Usage{Cost: cost, OwnCost: &cost}}})
		assert.Equal(t, i == 0, changed, "repeated/out-of-order cumulative replay does not increment or regress")
	}
	assert.Equal(t, 5.0, observedCost(t, watch, base, "grand"))
	watch.add(grand)
	assert.Equal(t, 5.0, observedCost(t, watch, base, "grand"), "older dynamic baseline cannot regress live usage")
	assert.False(t, watch.apply(runtime.SessionEvent{SessionID: "child", Event: &runtime.TokenUsageEvent{Usage: &runtime.Usage{Cost: 500}}}), "legacy aggregate relies on canonical/snapshot own cost")
	assert.Equal(t, 2.0, observedCost(t, watch, base, "child"))
	grand = ownCostSnapshot("grand", "child", 5)
	for range 2 {
		watch.reset([]runtime.SessionSnapshot{root, child, grand})
		assert.Equal(t, 5.0, observedCost(t, watch, base, "grand"), "reconnect baseline replaces rather than adds")
	}
}

// Exercise the session-v2 SSE decoder, ordered tree replay and live projection
// without providers or a running Kit instance.
func TestRemoteTreeOwnCostWireReplayAndLive(t *testing.T) {
	t.Parallel()
	baseline := ownCostSnapshot("root", "", 1)
	tree := subagent.Snapshot{Root: "root:root", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:root", Agent: "root", Cost: 1}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Agent: "worker", SessionID: "child", Cost: 2, State: subagent.NodeCompleted}}}}}}
	baseline.Session.SetSubagentTree(&tree)
	rootJSON, err := json.Marshal(baseline.Session)
	require.NoError(t, err)
	childJSON, err := json.Marshal(ownCostSnapshot("child", "root", 2).Session)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "true", r.URL.Query().Get("tree"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":%s,\"status\":{\"session_id\":\"root\"}}}\n\n", rootJSON)
		fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":%s,\"status\":{\"session_id\":\"child\"}}}\n\n", childJSON)
		for _, frame := range []string{
			`{"version":2,"type":"event","envelope":{"version":2,"session_id":"child","sequence":1,"event":{"type":"token_usage","session_id":"child","agent_name":"worker","usage":{"cost":30,"own_cost":3}}}}`,
			`{"version":2,"type":"ready"}`,
			`{"version":2,"type":"event","envelope":{"version":2,"session_id":"child","sequence":2,"event":{"type":"token_usage","session_id":"child","agent_name":"worker","usage":{"cost":40,"own_cost":4}}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	}))
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("root")
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), runtime.ObserveOptions{Tree: true, OrderedTree: true})
	require.NoError(t, err)
	defer obs.Cancel()
	watch := newTreeWatch("root")
	watch.reset(obs.Initial)
	require.Len(t, obs.Replay, 1)
	watch.apply(obs.Replay[0])
	assert.Equal(t, 3.0, observedCost(t, watch, &tree, "child"))
	update := <-obs.TreeUpdates
	require.NotNil(t, update.Event)
	watch.apply(*update.Event)
	assert.Equal(t, 4.0, observedCost(t, watch, &tree, "child"))
	assert.Equal(t, 1.0, observedCost(t, watch, &tree, "root"), "idle ancestor own cost stays unchanged")
	watch.apply(obs.Replay[0])
	assert.Equal(t, 4.0, observedCost(t, watch, &tree, "child"), "replay cannot regress latest live spend")
}
