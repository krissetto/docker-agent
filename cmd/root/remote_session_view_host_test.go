package root

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// Exercise the managed backend's hosted-view route, not direct preparation.
func TestManagedRemoteHostedChildView(t *testing.T) {
	const source = "team.yaml"
	root := session.New(session.WithID("root"), session.WithAgentName("director"), session.WithWorkingDir("/server/workspace"), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source}))
	child := session.New(session.WithID("child"), session.WithAgentName("worker"), session.WithWorkingDir(root.WorkingDir), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source}))
	child.ParentID = root.ID
	child.AddMessage(session.UserMessage("server-owned history"))
	info := runtime.PreparedSessionViewInfo{ActiveAgentName: "active-worker", SessionID: child.ID, RootSessionID: root.ID, Session: child, WorkingDir: child.WorkingDir, Binding: runtime.SessionBinding{AgentName: "worker", ParentSessionID: root.ID}, Attach: &runtime.SubagentAttachInfo{NodeID: "child-node", Agent: "active-worker", ParentSessionID: root.ID, ParentAgent: "director", Session: child.Clone()}}
	rootInfo := runtime.PreparedSessionViewInfo{ActiveAgentName: "active-director", SessionID: root.ID, RootSessionID: root.ID, Session: root, WorkingDir: root.WorkingDir, Binding: runtime.SessionBinding{AgentName: "director"}}
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("view") == "prepare-info":
			selected := info
			if r.URL.Path == api.SessionAPIPath+"/root" {
				selected = rootInfo
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": selected})
		case r.Method == http.MethodPatch:
			var edit runtime.SessionEdit
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&edit)) {
				return
			}
			assert.Equal(t, runtime.SessionEditOpenView, edit.Kind)
			if r.URL.Path == api.SessionAPIPath+"/root" {
				writeReadyOpening(t, w, rootInfo)
			} else {
				writeReadyOpening(t, w, info)
			}
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/root/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": api.SessionMetadata{SessionID: root.ID, AgentName: "active-director"}, "status": runtime.SessionStatus{SessionID: root.ID, AgentName: "active-director", State: runtime.SessionStateSettled}})
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/root/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{"session": root, "status": runtime.SessionStatus{SessionID: root.ID, AgentName: "director", State: runtime.SessionStateSettled}})
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/child/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": api.SessionMetadata{SessionID: child.ID, AgentName: "active-worker"}, "status": runtime.SessionStatus{SessionID: child.ID, AgentName: "worker", State: runtime.SessionStateSettled}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	flags := &runExecFlags{remoteAddress: srv.URL, sessionID: root.ID}
	backend := &remoteBackend{flags: flags, agentFileName: source}
	services, sessions, initial, cleanup, err := backend.CreateSession(t.Context(), nil, backend.CreateSessionRequest("/client/workspace"))
	require.NoError(t, err)
	transport, ok := sessions.(*runtime.SessionTransport)
	require.True(t, ok)
	require.NoError(t, flags.configureSessionViewHost(t.Context(), backend, services, transport, initial, nil, cleanup))
	defer flags.sessionViewHost.Shutdown()
	prepared, err := flags.sessionViewHost.AcquireSessionView(t.Context(), child.ID)
	require.NoError(t, err)
	defer prepared.Abort()
	mu.Lock()
	assert.NotContains(t, requests, "PATCH "+api.SessionAPIPath+"/child", "resolution and preparation are read-only")
	mu.Unlock()
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	view, err := prepared.NewApp(t.Context(), committed)
	require.NoError(t, err)
	assert.Same(t, transport, view.SessionRuntime())
	assert.False(t, runtime.IsLocalSessionHandle(view.SessionHandle()))
	require.NotNil(t, view.AttachedSubagent())
	assert.Equal(t, info.Attach.NodeID, view.AttachedSubagent().NodeID)
	assert.Equal(t, "worker", view.Binding().AgentName)
	assert.Equal(t, "active-worker", view.Session().AgentName)
	assert.Equal(t, "active-worker", view.AttachedSubagent().Agent)
	assert.Equal(t, child.MessagesSnapshot(), view.Session().MessagesSnapshot())
	view.Close()
	prepared.Abort()
	// Closing a borrowed view must never cancel, release, or stop server work.
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requests {
		assert.NotContains(t, request, "cancel")
		assert.NotContains(t, request, "stop")
		assert.NotContains(t, request, "release")
		assert.NotContains(t, request, "DELETE")
	}
}
