package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestAttachServerRequiresArgs(t *testing.T) {
	t.Parallel()
	_, err := AttachServer(t.Context(), "", "session-1")
	require.ErrorContains(t, err, "addr and sessionID")
	_, err = AttachServer(t.Context(), "http://127.0.0.1:1234", "")
	require.ErrorContains(t, err, "addr and sessionID")
	_, err = AttachServer(t.Context(), "https://secret@example.invalid", "one")
	require.ErrorContains(t, err, "must not contain credentials")
}

func TestAttachAuthenticatedCanonicalControls(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	responses := map[string]int{}
	cancels := map[string]int{}
	stops := map[string]int{}
	enabled := true
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, api.SessionAPIPath+"/"), "/")
		if len(parts) < 2 || (parts[0] != "one" && parts[0] != "two") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id, operation := parts[0], parts[1]
		mu.Lock()
		defer mu.Unlock()
		encode := func(value any) { assert.NoError(t, json.NewEncoder(w).Encode(value)) }
		switch operation {
		case "snapshot":
			encode(api.SessionSnapshot[string, string, map[string]any]{Session: &session.Session{ID: id, Title: id}, Status: api.SessionStatus[string]{SessionID: id, State: "running", TurnID: id + "-turn"}, Epoch: "epoch", Interactions: []api.SessionInteraction[string, map[string]any]{
				{SessionID: id, InteractionID: id + "-form", ElicitationID: id + "-elicitation", Kind: "elicitation", Event: map[string]any{"type": "elicitation_request", "session_id": id, "request_id": id + "-form", "elicitation_id": id + "-elicitation", "mode": "form", "schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}}}},
				{SessionID: id, InteractionID: id + "-approval", Kind: "confirmation", Event: map[string]any{"type": "tool_call_confirmation", "session_id": id, "request_id": id + "-approval"}},
				{SessionID: id, InteractionID: id + "-iterations", Kind: "max_iterations", Event: map[string]any{"type": "max_iterations_reached", "session_id": id, "request_id": id + "-iterations"}},
			}})
		case "status":
			encode(map[string]any{"metadata": api.SessionMetadata{SessionID: id, AgentName: "root", Capabilities: api.SessionCapabilities{StopSubtree: id == "one", DelegationPolicy: id == "one"}}, "status": api.SessionStatus[string]{SessionID: id, State: "running"}})
		case "messages":
			var request api.SessionInputRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				return
			}
			assert.Equal(t, "retry-key", request.RequestID)
			w.WriteHeader(http.StatusAccepted)
			encode(api.SessionSubmission[string]{SessionID: id, TurnID: id + "-turn", Disposition: "queued"})
		case "responses":
			var request api.SessionResponseRequest[string]
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				return
			}
			valid := (request.Kind == "elicitation" && request.InteractionID == id+"-form" && request.ElicitationID == id+"-elicitation" && request.Action == "accept" && request.Content["answer"] == "yes") || (request.Kind == "confirmation" && request.InteractionID == id+"-approval" && request.Confirmation == "reject") || (request.Kind == "max_iterations" && request.InteractionID == id+"-iterations" && request.Confirmation == "approve")
			if !valid {
				w.WriteHeader(http.StatusPreconditionFailed)
				encode(map[string]string{"error": "stale"})
				return
			}
			responses[id]++
			w.WriteHeader(http.StatusNoContent)
		case "cancel":
			var request api.SessionCancelRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				return
			}
			outcome := "not_active"
			if request.TurnID == id+"-turn" {
				cancels[id]++
				outcome = "accepted"
			}
			encode(map[string]string{"session_id": id, "turn_id": request.TurnID, "outcome": outcome})
		case "turns":
			assert.Equal(t, []string{id, "turns", id + "-turn", "wait"}, parts)
			w.WriteHeader(http.StatusNoContent)
		case "stop-subtree":
			stops[id]++
			w.WriteHeader(http.StatusNoContent)
		case "delegation-policy":
			if r.Method == http.MethodPatch {
				var request struct {
					Enabled bool `json:"enabled"`
				}
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
					return
				}
				enabled = request.Enabled
			}
			encode(map[string]bool{"enabled": enabled})
		default:
			t.Errorf("unexpected operation %s", operation)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer authority.Close()
	_, err := AttachServerWithOptions(t.Context(), authority.URL, "one", AttachOptions{ClientOptions: []runtime.ClientOption{runtime.WithAuthToken("wrong")}})
	require.Error(t, err)
	connect := func(id string) *gomcp.ClientSession {
		server, err := AttachServerWithOptions(t.Context(), authority.URL, id, AttachOptions{ClientOptions: []runtime.ClientOption{runtime.WithAuthToken("secret")}})
		require.NoError(t, err)
		ct, st := gomcp.NewInMemoryTransports()
		ss, err := server.Connect(t.Context(), st, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ss.Close() })
		client := gomcp.NewClient(&gomcp.Implementation{Name: "test", Version: "1"}, nil)
		cs, err := client.Connect(t.Context(), ct, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	one, two := connect("one"), connect("two")
	call := func(cs *gomcp.ClientSession, name string, args map[string]any) *gomcp.CallToolResult {
		out, err := cs.CallTool(t.Context(), &gomcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return out
	}
	for _, cs := range []*gomcp.ClientSession{one, two} {
		listed, err := cs.ListTools(t.Context(), nil)
		require.NoError(t, err)
		names := []string{}
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		assert.Contains(t, names, "respond")
		assert.Contains(t, names, "read")
		assert.Contains(t, names, "await_turn")
		if cs == one {
			assert.Contains(t, names, "stop_subtree")
			assert.Contains(t, names, "delegation_policy")
		} else {
			assert.NotContains(t, names, "stop_subtree")
			assert.NotContains(t, names, "delegation_policy")
		}
	}
	accepted := call(one, "send", map[string]any{"message": "hello", "request_id": "retry-key"})
	require.False(t, accepted.IsError)
	data, err := json.Marshal(accepted.StructuredContent)
	require.NoError(t, err)
	var admission runtime.Submission
	require.NoError(t, json.Unmarshal(data, &admission))
	assert.Equal(t, runtime.Submission{SessionID: "one", TurnID: "one-turn", Disposition: "queued"}, admission)
	read := call(one, "read", map[string]any{})
	require.False(t, read.IsError)
	data, err = json.Marshal(read.StructuredContent)
	require.NoError(t, err)
	var snapshot AttachedReadOutput
	require.NoError(t, json.Unmarshal(data, &snapshot))
	require.Len(t, snapshot.Interactions, 3)
	assert.Equal(t, "one-form", snapshot.Interactions[0].InteractionID)
	assert.NotNil(t, snapshot.Interactions[0].Event["schema"])
	for _, request := range []map[string]any{
		{"interaction_id": "one-form", "kind": "elicitation", "elicitation_id": "one-elicitation", "action": "accept", "content": map[string]any{"answer": "yes"}},
		{"interaction_id": "one-approval", "kind": "confirmation", "confirmation": "reject"},
		{"interaction_id": "one-iterations", "kind": "max_iterations", "confirmation": "approve"},
	} {
		require.False(t, call(one, "respond", request).IsError)
	}
	require.True(t, call(two, "respond", map[string]any{"interaction_id": "one-approval", "kind": "confirmation", "confirmation": "approve"}).IsError)
	require.True(t, call(one, "respond", map[string]any{"interaction_id": "stale", "kind": "confirmation", "confirmation": "approve"}).IsError)
	require.False(t, call(two, "cancel_turn", map[string]any{"turn_id": "one-turn"}).IsError)
	require.True(t, call(one, "cancel_turn", map[string]any{"turn_id": ""}).IsError)
	require.False(t, call(one, "await_turn", map[string]any{"turn_id": "one-turn"}).IsError)
	require.False(t, call(one, "cancel_turn", map[string]any{"turn_id": "one-turn"}).IsError)
	require.False(t, call(one, "delegation_policy", map[string]any{"enabled": false}).IsError)
	require.False(t, call(one, "stop_subtree", map[string]any{}).IsError)
	_, err = two.CallTool(t.Context(), &gomcp.CallToolParams{Name: "stop_subtree", Arguments: map[string]any{}})
	require.Error(t, err)
	require.NoError(t, one.Close())
	require.NoError(t, two.Close())
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, map[string]int{"one": 3}, responses)
	assert.Equal(t, map[string]int{"one": 1}, cancels, "disconnect never cancels")
	assert.Equal(t, map[string]int{"one": 1}, stops)
	assert.False(t, enabled)
}

func TestAttachRejectsUnconfirmedChildAndRedirect(t *testing.T) {
	t.Parallel()
	var foreignRequests int
	var mu sync.Mutex
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		foreignRequests++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer foreign.Close()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "redirect") {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(api.SessionSnapshot[string, string, json.RawMessage]{ParentSessionID: "root", Session: &session.Session{ID: "child"}, Status: api.SessionStatus[string]{SessionID: "child"}}))
	}))
	defer authority.Close()
	_, err := AttachServer(t.Context(), authority.URL, "child")
	require.ErrorContains(t, err, "explicit confirmation")
	_, err = AttachServerWithOptions(t.Context(), authority.URL, "redirect", AttachOptions{ClientOptions: []runtime.ClientOption{runtime.WithAuthToken("secret")}})
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, foreignRequests)
}
