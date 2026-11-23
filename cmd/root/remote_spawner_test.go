package root

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestManagedNewTabUsesDefaultAgentInOneRequest(t *testing.T) {
	const source, workspace = "team.yaml", "/server/workspace"
	original := session.New(session.WithID("original"), session.WithAgentName("root"), session.WithWorkingDir(workspace), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source}))
	switched := session.New(session.WithID("switched"), session.WithAgentName("worker"), session.WithWorkingDir(workspace), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source}), session.WithUserMessage("retained history"))
	fresh := session.New(session.WithID("fresh"), session.WithAgentName("root"), session.WithWorkingDir(workspace), session.WithSafetyPolicy(session.SafetyPolicyBalanced), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source}))
	info := func(sess *session.Session) runtime.PreparedSessionViewInfo {
		return runtime.PreparedSessionViewInfo{SessionID: sess.ID, RootSessionID: sess.ID, Session: sess, WorkingDir: sess.WorkingDir, ActiveAgentName: sess.AgentName, Binding: runtime.SessionBinding{AgentName: sess.AgentName}}
	}
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		encode := func(value any) { assert.NoError(t, json.NewEncoder(w).Encode(value)) }
		switch r.Method + " " + r.URL.Path {
		case "POST " + api.SessionAPIPath + "/original/switch-agent":
			var req struct {
				AgentName string `json:"agent_name"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
				return
			}
			assert.Equal(t, "worker", req.AgentName)
			encode(map[string]any{"metadata": api.SessionMetadata{SessionID: switched.ID, AgentName: "worker", Model: "saved/worker"}, "session": switched})
		case "GET " + api.SessionAPIPath + "/original":
			encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": info(original)})
		case "PATCH " + api.SessionAPIPath + "/original":
			writeReadyOpening(t, w, info(original))
		case "GET " + api.SessionAPIPath + "/switched":
			encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": info(switched)})
		case "PATCH " + api.SessionAPIPath + "/switched":
			opening := info(switched)
			opening.Binding.Model = "saved/worker"
			writeReadyOpening(t, w, opening)
		case "GET /api/agents/" + source:
			fmt.Fprint(w, `{"agents":[{"name":"worker"},{"name":"root"}]}`)
		case "POST " + api.SessionAPIPath:
			var req api.SessionCreateRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
				return
			}
			assert.Empty(t, req.AgentName, "the server chooses the default, not the initial view")
			assert.Empty(t, req.Model, "a fresh tab must not inherit the resumed model")
			assert.Equal(t, source, req.Source)
			assert.Equal(t, workspace, req.WorkingDir)
			encode(api.SessionCreated{SessionMetadata: api.SessionMetadata{SessionID: fresh.ID, AgentName: "root", Model: "team/root"}, Session: fresh})
		case "GET " + api.SessionAPIPath + "/fresh/snapshot":
			encode(map[string]any{"session": fresh, "status": map[string]any{"session_id": fresh.ID}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	flags := &runExecFlags{managedAPI: true, remoteAddress: srv.URL, remoteWorkingDir: workspace, sessionID: original.ID}
	backend := &remoteBackend{flags: flags, agentFileName: source}
	services, sessions, initial, cleanup, err := backend.CreateSession(t.Context(), nil, backend.CreateSessionRequest(""))
	require.NoError(t, err)
	t.Cleanup(cleanup)
	originApp, err := app.NewResolved(t.Context(), sessions, runtime.CommittedSessionView{SessionHandle: backend.handle, Info: info(initial)}, app.WithRuntimeServices(services))
	require.NoError(t, err)
	t.Cleanup(originApp.Close)
	_, _, err = originApp.SwitchAgentTransactional(t.Context(), "worker")
	require.NoError(t, err)
	require.Equal(t, switched.ID, originApp.Session().ID)
	require.Equal(t, "worker", originApp.SessionHandle().AgentName())

	// Resume the switched identity in a new client. Its source-wide services
	// are intentionally seeded from the restored worker, which used to leak
	// into the next tab's selection.
	resumeFlags := &runExecFlags{managedAPI: true, remoteAddress: srv.URL, remoteWorkingDir: workspace, sessionID: switched.ID}
	resumedBackend := &remoteBackend{flags: resumeFlags, agentFileName: source}
	resumedServices, resumedSessions, resumed, cleanup, err := resumedBackend.CreateSession(t.Context(), nil, resumedBackend.CreateSessionRequest(""))
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.Equal(t, "worker", resumed.AgentName)
	require.Len(t, resumed.MessagesSnapshot(), 1)
	spawner := resumedBackend.Spawner(resumedServices, resumedSessions)
	mu.Lock()
	requests = nil
	mu.Unlock()
	spawned, err := spawner(t.Context(), "")
	require.NoError(t, err)
	t.Cleanup(spawned.App.Close)
	mu.Lock()
	got := append([]string(nil), requests...)
	mu.Unlock()
	assert.Equal(t, []string{"POST " + api.SessionAPIPath + "?view=created"}, got)
	assert.Equal(t, "root", spawned.App.Binding().AgentName)
	assert.Equal(t, "team/root", spawned.App.Binding().Model)
	assert.Equal(t, session.SafetyPolicyBalanced, spawned.Session.GetSafetyPolicy())
	assert.Empty(t, spawned.Session.MessagesSnapshot())
	assert.Equal(t, "worker", resumed.AgentName, "new tab must not change other tabs")
	assert.Equal(t, "worker", resumedBackend.handle.AgentName())
	assert.Equal(t, "worker", originApp.SessionHandle().AgentName(), "new tab must not change the switched tab")
}

func TestRemoteNewTabWithLegacyCreateResponse(t *testing.T) {
	const source, workspace = "team.yaml", "/server/workspace"
	fresh := session.New(session.WithID("fresh"), session.WithWorkingDir(workspace), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/agents/" + source:
			fmt.Fprint(w, `{"agents":[{"name":"worker"},{"name":"root"}]}`)
		case "POST " + api.SessionAPIPath:
			var req api.SessionCreateRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
				return
			}
			assert.Equal(t, "root", req.AgentName)
			assert.Equal(t, "created", r.URL.Query().Get("view"))
			assert.NoError(t, json.NewEncoder(w).Encode(api.SessionMetadata{SessionID: fresh.ID, AgentName: "root"}))
		case "GET " + api.SessionAPIPath + "/fresh/snapshot":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session": fresh, "status": map[string]any{"session_id": fresh.ID}}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := runtime.NewClient(srv.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(source))
	require.NoError(t, err)
	services, err := runtime.NewRemoteServices(client, runtime.WithRemoteAgentFilename(source), runtime.WithRemoteCurrentAgent("worker"))
	require.NoError(t, err)
	b := &remoteBackend{flags: &runExecFlags{remoteWorkingDir: workspace}, agentFileName: source}
	spawned, err := b.Spawner(services, transport)(t.Context(), "")
	require.NoError(t, err)
	t.Cleanup(spawned.App.Close)
	assert.Equal(t, "root", spawned.App.Binding().AgentName)
	assert.Equal(t, workspace, spawned.Session.WorkingDir)
}
