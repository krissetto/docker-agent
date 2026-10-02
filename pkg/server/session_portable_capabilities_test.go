package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func newPortableServer(t *testing.T) (*Server, session.Store) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(sessionHTTPProvider{}), agent.WithToolSets(todotool.New())))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	return NewWithManager(sm, "token", WithServerInfo(api.ServerInfo{WorkspaceRoot: "/workspace", Source: "team.yaml", ConfigFingerprint: "digest"})), store
}

func TestPortableServerIdentityAuthenticatedWithoutSessions(t *testing.T) {
	srv, store := newPortableServer(t)
	unauthorized := sessionRequest(t, srv, http.MethodGet, api.ServerInfoPath, "", "")
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	rec := sessionRequest(t, srv, http.MethodGet, api.ServerInfoPath, "", "token")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var info api.ServerInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	require.True(t, info.Ready)
	require.NotEmpty(t, info.InstanceID)
	require.Equal(t, "/workspace", info.WorkspaceRoot)
	require.Equal(t, "digest", info.ConfigFingerprint)
	require.Contains(t, info.Capabilities, "session_branching")
	rows, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	require.Empty(t, rows)
	other, _ := newPortableServer(t)
	require.NotEqual(t, srv.serverInfo.InstanceID, other.serverInfo.InstanceID)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithAuthToken("token"))
	require.NoError(t, err)
	fetched, err := client.ServerInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, info, fetched)
}

func TestPortableHTTPTodosCASAndBranch(t *testing.T) {
	srv, store := newPortableServer(t)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithAuthToken("token"))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	h, err := transport.CreateSessionWithID(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"}, "explicit-id")
	require.NoError(t, err)
	require.Equal(t, "explicit-id", h.ID())
	_, err = transport.CreateSessionWithID(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"}, h.ID())
	var typed *runtime.SessionError
	require.ErrorAs(t, err, &typed)
	require.Equal(t, runtime.SessionErrorConflict, typed.Kind)
	require.NoError(t, store.(session.TodoStore).SaveTodos(t.Context(), h.ID(), []session.Todo{{ID: "one", Description: "", Status: "pending"}}))
	base := api.SessionAPIPath + "/" + h.ID()
	missing := sessionRequest(t, srv, http.MethodPatch, base+"/todos/one", `{"description":"new"}`, "token")
	require.Equal(t, http.StatusBadRequest, missing.Code)
	todos, err := h.SetTodoDescription(t.Context(), "one", "", "new")
	require.NoError(t, err)
	require.Equal(t, "new", todos[0].Description)
	_, err = h.SetTodoDescription(t.Context(), "one", "", "stale")
	require.ErrorAs(t, err, &typed)
	require.Equal(t, runtime.SessionErrorConflict, typed.Kind)
	todos, err = h.SetTodoStatus(t.Context(), "one", "completed")
	require.NoError(t, err)
	require.Equal(t, "completed", todos[0].Status)
	presentation, err := h.(runtime.SessionAgentInfoProvider).SessionAgentInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, "root", presentation.Agent.AgentName)
	require.Equal(t, "test/http", presentation.Agent.Model)
	var emitted []runtime.Event
	h.EmitPinnedAgentInfo(t.Context(), runtime.EventSinkFunc(func(event runtime.Event) { emitted = append(emitted, event) }))
	require.Len(t, emitted, 2)
	tools, err := h.(runtime.SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, tools.Tools)
	perms, err := h.(runtime.SessionPermissionsInspector).EffectivePermissions(t.Context())
	require.NoError(t, err)
	require.False(t, perms.ToolsApproved)
	prompts, err := h.(runtime.SessionMCPPrompts).MCPPrompts(t.Context())
	require.NoError(t, err)
	require.Empty(t, prompts)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	zero := 0
	branch, child, err := transport.BranchSession(t.Context(), h.ID(), runtime.BranchOptions{Position: &zero, ExpectedSnapshot: runtime.SnapshotProof(snapshot)})
	require.NoError(t, err)
	require.NotEqual(t, h.ID(), branch.ID())
	require.Empty(t, child.Messages)
	todos, err = h.RemoveTodo(t.Context(), "one")
	require.NoError(t, err)
	require.Empty(t, todos)
}

func TestPortableDetachSecondClientCompletionAndReattach(t *testing.T) {
	srv, _ := newPortableServer(t)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	firstClient, err := runtime.NewClient(server.URL, runtime.WithAuthToken("token"))
	require.NoError(t, err)
	first, err := runtime.NewSessionTransport(firstClient)
	require.NoError(t, err)
	handle, err := first.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	submission, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "offline fake turn"})
	require.NoError(t, err)
	observation.Cancel() // detaches only; no cancel mutation
	secondClient, err := runtime.NewClient(server.URL, runtime.WithAuthToken("token"))
	require.NoError(t, err)
	second, err := runtime.NewSessionTransport(secondClient)
	require.NoError(t, err)
	peer, err := second.SessionByID(handle.ID())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, peer.AwaitTurn(ctx, submission.TurnID))
	reattached, err := peer.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer reattached.Cancel()
	snapshot := reattached.Primary().Session
	require.NotNil(t, snapshot)
	users, replies := 0, 0
	for _, msg := range snapshot.GetAllMessages() {
		if msg.Message.Content == "offline fake turn" {
			users++
		}
		if strings.Contains(msg.Message.Content, "HTTP integration reply") {
			replies++
		}
	}
	require.Equal(t, 1, users)
	require.Equal(t, 1, replies)
}

func TestPortableCreateIDValidationAndConcurrentConflict(t *testing.T) {
	srv, _ := newPortableServer(t)
	for _, id := range []string{"../escape", "a%2Fb", "a\nb", "a b", "a?b"} {
		body, _ := json.Marshal(api.SessionCreateRequest{SessionID: id, AgentName: "root"})
		rec := sessionRequest(t, srv, http.MethodPost, api.SessionAPIPath, string(body), "token")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	var codes = make(chan int, 2)
	for range 2 {
		go func() {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, api.SessionAPIPath, strings.NewReader(`{"session_id":"concurrent","agent_name":"root"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer token")
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
	}
	require.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict}, []int{<-codes, <-codes})
}
