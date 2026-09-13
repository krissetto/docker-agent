package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

type modelUpdateFailStore struct {
	session.Store

	err  error
	fail bool
}

func (s *modelUpdateFailStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if s.fail {
		return s.err
	}
	return s.Store.UpdateSession(ctx, sess)
}

func newCanonicalLocalServer(t *testing.T, store session.Store, agents ...*agent.Agent) (*Server, runtime.SessionRuntimeSupervisor) {
	t.Helper()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agents...)), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	return NewWithManager(sm, ""), owner
}

func TestCanonicalAuthorSafetyDefaultConsumedOnceAcrossAgentSwitch(t *testing.T) {
	store := session.NewInMemorySessionStore()
	model := sessionHTTPProvider{}
	srv, _ := newCanonicalLocalServer(t, store,
		agent.New("root", "prompt", agent.WithModel(model), agent.WithSafety(latest.SafetyModeAutonomous)),
		agent.New("worker", "prompt", agent.WithModel(model), agent.WithSafety(latest.SafetyModeStrict)),
	)

	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"agent_name":"root"}`, "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))
	original, err := store.GetSession(t.Context(), metadata.SessionID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, original.GetSafetyPolicy())

	switched := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+metadata.SessionID+"/switch-agent", `{"agent_name":"worker"}`, "")
	require.Equal(t, http.StatusCreated, switched.Code, switched.Body.String())
	var response struct {
		Metadata sessionMetadataDTO `json:"metadata"`
		Session  *session.Session   `json:"session"`
	}
	require.NoError(t, json.Unmarshal(switched.Body.Bytes(), &response))
	require.NotNil(t, response.Session)
	assert.Equal(t, "worker", response.Metadata.AgentName)
	assert.Equal(t, session.SafetyPolicyAutonomous, response.Session.GetSafetyPolicy(), "switching must preserve the consumed root default, not apply the worker default")
	persisted, err := store.GetSession(t.Context(), response.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, persisted.GetSafetyPolicy())
}

func TestCanonicalSessionModelRollbackOnStoreFailure(t *testing.T) {
	baseStore := session.NewInMemorySessionStore()
	persistErr := errors.New("disk full")
	store := &modelUpdateFailStore{Store: baseStore, err: persistErr}
	model := sessionHTTPProvider{}
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(model)))),
		runtime.WithSessionStore(store), runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{EnvProvider: environment.NewMapEnvProvider(nil)}))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	srv := NewWithManager(sm, "")

	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"agent_name":"root"}`, "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))
	stored, err := baseStore.GetSession(t.Context(), metadata.SessionID)
	require.NoError(t, err)
	stored.AgentModelOverrides = map[string]string{"root": "provider/old"}
	stored.CustomModelsUsed = []string{"provider/old"}
	store.fail = true

	failed := sessionRequest(t, srv, http.MethodPatch, "/api/sessions/"+metadata.SessionID+"/model", `{"model":""}`, "")
	assert.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
	handle, err := owner.Runtime().SessionByID(metadata.SessionID)
	require.NoError(t, err)
	assert.Empty(t, handle.Metadata().Model, "failed persistence must not publish the requested model")
	assert.Equal(t, "provider/old", stored.AgentModelOverrides["root"])
	assert.Equal(t, []string{"provider/old"}, stored.CustomModelsUsed)
}

func TestCanonicalSessionsReadyImmediateAfterCreateAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		create     bool
		wantStatus int
	}{
		{name: "canonical create establishes readiness", create: true, wantStatus: http.StatusOK},
		{name: "timeout", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
			srv, _ := newSessionHTTPServer(t, registry)
			if tc.create {
				created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"agent_name":"root"}`, "")
				require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			}
			started := time.Now()
			rec := sessionRequest(t, srv, http.MethodGet, "/api/ready?timeout=5ms", "", "")
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			assert.Less(t, time.Since(started), time.Second)
		})
	}
}

func TestCanonicalSessionEventsEmitHeartbeatAfterSnapshotAndReady(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	handle := &httpSession{id: "heartbeat", agent: "root", attach: runtime.Observation{
		Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID("heartbeat"))}},
		Events:  events, Cancel: func() {},
	}}
	srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{"heartbeat": handle}})
	srv.heartbeatInterval = 5 * time.Millisecond
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/sessions/heartbeat/events", http.NoBody)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	lines := make(chan string, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				readErr <- err
				return
			}
			lines <- strings.TrimSpace(line)
		}
	}()
	var eventsSeen []string
	for {
		select {
		case line := <-lines:
			if event, ok := strings.CutPrefix(line, "event: "); ok {
				eventsSeen = append(eventsSeen, event)
			}
			if line == ": ping" {
				assert.Equal(t, []string{"snapshot", "ready"}, eventsSeen)
				return
			}
		case err := <-readErr:
			t.Fatalf("event stream ended before heartbeat: %v", err)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for heartbeat")
		}
	}
}

func TestCanonicalDeleteOpenObserverEmitsDeletedThenEOF(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"agent_name":"root"}`, "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()

	res, err := http.Get(httpServer.URL + "/api/sessions/" + metadata.SessionID + "/events") //nolint:noctx // bounded local test server
	require.NoError(t, err)
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	for frames := 0; frames < 2; {
		line, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		if strings.HasPrefix(line, "data: ") {
			frames++
		}
	}

	deleted := sessionRequest(t, srv, http.MethodDelete, "/api/sessions/"+metadata.SessionID, "", "")
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
	var terminal string
	for {
		line, readErr := reader.ReadString('\n')
		if strings.HasPrefix(line, "data: ") {
			terminal = line
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		require.NoError(t, readErr)
	}
	assert.Contains(t, terminal, `"type":"stream_stopped"`)
	assert.Contains(t, terminal, `"reason":"deleted"`)
}

func TestCanonicalForkSiblingNumberingAndNestedDeepCopy(t *testing.T) {
	store := session.NewInMemorySessionStore()
	image := &chat.MessageImageURL{URL: "http://parent"}
	document := &chat.Document{Name: "parent.txt", Source: chat.DocumentSource{InlineData: []byte("parent")}}
	parent := session.New(session.WithID("parent"), session.WithTitle("Original"), session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessage("first", chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: image}, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: document})),
		session.NewMessageItem(session.NewAgentMessage("root", &session.UserMessage("reply").Message)),
		session.NewMessageItem(session.UserMessage("second")),
	}))
	require.NoError(t, store.AddSession(t.Context(), parent))
	srv := NewWithManager(NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}), "")

	forkIDs := make([]string, 0, 2)
	for i, wantTitle := range []string{"Original (fork 1)", "Original (fork 2)"} {
		rec := sessionRequest(t, srv, http.MethodPost, "/api/sessions/parent/fork", `{"user_message_index":1}`, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var fork api.SessionResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fork))
		assert.Equal(t, wantTitle, fork.Title, "fork %d", i+1)
		forkIDs = append(forkIDs, fork.ID)
	}
	forked, err := store.GetSession(t.Context(), forkIDs[0])
	require.NoError(t, err)
	parts := forked.Messages[0].Message.Message.MultiContent
	parts[0].ImageURL.URL = "http://mutated"
	parts[1].Document.Name = "mutated.txt"
	parts[1].Document.Source.InlineData[0] = 'M'
	parentParts := parent.Messages[0].Message.Message.MultiContent
	assert.Equal(t, "http://parent", parentParts[0].ImageURL.URL)
	assert.Equal(t, "parent.txt", parentParts[1].Document.Name)
	assert.Equal(t, []byte("parent"), parentParts[1].Document.Source.InlineData)
	other, err := store.GetSession(t.Context(), forkIDs[1])
	require.NoError(t, err)
	assert.Equal(t, "http://parent", other.Messages[0].Message.Message.MultiContent[0].ImageURL.URL)
}

func TestCanonicalSnapshotUnknownSessionFromSQLiteIsTyped(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	srv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	rec := sessionRequest(t, srv, http.MethodGet, "/api/sessions/missing/snapshot", "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var body struct {
		Error     string `json:"error"`
		Operation string `json:"operation"`
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, string(runtime.SessionErrorNotFound), body.Error)
	assert.Equal(t, "lookup", body.Operation)
	assert.Equal(t, "missing", body.SessionID)
}
