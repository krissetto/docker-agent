package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	configtypes "github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestLegacySessionMetadataUsesSameDurableRows(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(t.Context(), nil, store, 0, nil)
	srv := NewWithManager(sm, "secret")
	unauthorized := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"title":"old"}`, "")
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"title":"old","tools_approved":true}`, "secret")
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var row session.Session
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &row))
	require.NotEmpty(t, row.ID)
	assert.Zero(t, sm.runtimeSessions.Length(), "unbound template is metadata, not a second lifecycle")
	title := sessionRequest(t, srv, http.MethodPatch, "/api/sessions/"+row.ID+"/title", `{"title":"updated"}`, "secret")
	require.Equal(t, http.StatusOK, title.Code)
	var titleResponse api.UpdateSessionTitleResponse
	require.NoError(t, json.Unmarshal(title.Body.Bytes(), &titleResponse))
	assert.Equal(t, row.ID, titleResponse.ID)
	added := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+row.ID+"/messages", `{"message":{"message":{"role":"user","content":"history"}}}`, "secret")
	require.Equal(t, http.StatusCreated, added.Code, added.Body.String())
	detail := sessionRequest(t, srv, http.MethodGet, "/api/sessions/"+row.ID, "", "secret")
	require.Equal(t, http.StatusOK, detail.Code)
	var old api.SessionResponse
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &old))
	assert.Equal(t, "updated", old.Title)
	require.Len(t, old.Messages, 1)
	stored, err := store.GetSession(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, old.Messages[0].Message, stored.GetAllMessages()[0].Message)
	list := sessionRequest(t, srv, http.MethodGet, "/api/sessions", "", "secret")
	var rows []api.SessionsResponse
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, row.ID, rows[0].ID)
	snapshot := sessionRequest(t, srv, http.MethodGet, "/api/sessions/"+row.ID+"/snapshot", "", "secret")
	var snap api.SessionSnapshotResponse
	require.NoError(t, json.Unmarshal(snapshot.Body.Bytes(), &snap))
	assert.Equal(t, old.Messages, snap.Messages)
	assert.Zero(t, snap.LastEventSeq)
	missing := sessionRequest(t, srv, http.MethodGet, "/api/sessions/missing/snapshot", "", "secret")
	require.Equal(t, http.StatusNotFound, missing.Code)
	assert.Contains(t, missing.Body.String(), api.ErrCodeUnknownSession)
	active := sessionRequest(t, srv, http.MethodGet, "/api/sessions?active=true", "", "secret")
	assert.JSONEq(t, `[]`, active.Body.String())
}

func TestLegacyResponsesRequireUniqueCanonicalInteraction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prompts []runtime.InteractionSnapshot
		want    int
	}{
		{"none", nil, http.StatusConflict},
		{"one", []runtime.InteractionSnapshot{{SessionID: "s", InteractionID: "exact", Kind: runtime.InteractionConfirmation}}, http.StatusOK},
		{"ambiguous", []runtime.InteractionSnapshot{{SessionID: "s", InteractionID: "first", Kind: runtime.InteractionConfirmation}, {SessionID: "s", InteractionID: "second", Kind: runtime.InteractionMaxIterations}}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &httpSession{id: "s", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Interactions: tc.prompts}}, Cancel: func() {}}}
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{"s": h}}
			srv, _ := newSessionHTTPServer(t, registry)
			response := sessionRequest(t, srv, http.MethodPost, "/api/sessions/s/resume", `{"confirmation":"approve","reason":"ok"}`, "")
			require.Equal(t, tc.want, response.Code, response.Body.String())
			if tc.want == http.StatusOK {
				require.Len(t, h.responses, 1)
				assert.Equal(t, "exact", h.responses[0].InteractionID)
				assert.Equal(t, "exact", h.responses[0].Resume.RequestID)
			} else {
				assert.Empty(t, h.responses)
			}
			assert.Empty(t, h.cancels, "response projection never cancels an arbitrary turn")
		})
	}
}

func TestLegacyElicitationIDFiltersConcurrentPrompts(t *testing.T) {
	h := &httpSession{id: "s", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Interactions: []runtime.InteractionSnapshot{
		{SessionID: "s", InteractionID: "one", ElicitationID: "form-one", Kind: runtime.InteractionElicitation},
		{SessionID: "s", InteractionID: "two", ElicitationID: "form-two", Kind: runtime.InteractionElicitation},
	}}}, Cancel: func() {}}}
	srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{"s": h}})
	ambiguous := sessionRequest(t, srv, http.MethodPost, "/api/sessions/s/elicitation", `{"action":"accept"}`, "")
	assert.Equal(t, http.StatusConflict, ambiguous.Code)
	exact := sessionRequest(t, srv, http.MethodPost, "/api/sessions/s/elicitation", `{"action":"accept","elicitation_id":"form-two","content":{"answer":"yes"}}`, "")
	require.Equal(t, http.StatusOK, exact.Code, exact.Body.String())
	require.Len(t, h.responses, 1)
	assert.Equal(t, "two", h.responses[0].InteractionID)
	assert.Equal(t, "form-two", h.responses[0].ElicitationID)
}

func TestLegacyRunFactoryAdoptsUnboundTemplateIntoCanonicalOwner(t *testing.T) {
	store := session.NewInMemorySessionStore()
	factory := &factoryRecorder{store: store}
	source := &memorySource{data: "version: '2'\nagents:\n  root:\n    model: test/model\n    instruction: test\n"}
	srv, sm := newFactoryServer(t, factory, source)
	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"title":"legacy factory"}`, "")
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var row session.Session
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &row))
	run := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+row.ID+"/agent/agent", `{"messages":[{"role":"assistant","content":"first"},{"role":"user","content":"second"}]}`, "")
	require.Equal(t, http.StatusOK, run.Code, run.Body.String())
	assert.Contains(t, run.Body.String(), "data:")
	assert.NotContains(t, run.Body.String(), `"envelope":`)
	h, err := sm.Handle(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, "root", h.AgentName())
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "agent", snapshot.AttributesSnapshot()[sessionSourceAttribute])
	var contents []string
	for _, message := range snapshot.GetAllMessages() {
		if message.Message.Content == "first" || message.Message.Content == "second" {
			contents = append(contents, message.Message.Content)
			assert.Equal(t, chat.MessageRoleUser, message.Message.Role)
		}
	}
	assert.Equal(t, []string{"first", "second"}, contents)
	canonical := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+row.ID+"/snapshot", "", "")
	require.Equal(t, http.StatusOK, canonical.Code, canonical.Body.String())
	assert.Contains(t, canonical.Body.String(), row.ID)
	stored, err := store.GetSession(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", stored.AttributesSnapshot()[sessionAgentAttribute], "binding must survive restart")
	assert.Equal(t, "agent", stored.AttributesSnapshot()[sessionSourceAttribute], "source must survive restart")
	// Empty old messages starts one execution against existing history.
	again := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+row.ID+"/agent/ignored/ignored", `{"messages":[]}`, "")
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	assert.Len(t, factory.built(), 1, "old and canonical requests share one workspace runtime")
}

func TestLegacyUnsupportedModelDoesNotAppendInput(t *testing.T) {
	store := session.NewInMemorySessionStore()
	factory := &factoryRecorder{store: store}
	source := &memorySource{data: "version: '2'\nagents:\n  root:\n    model: test/model\n    instruction: test\n    commands:\n      plan:\n        agent: worker\n  worker:\n    model: test/model\n    instruction: test\n"}
	srv, sm := newFactoryServer(t, factory, source)
	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{}`, "")
	require.Equal(t, http.StatusOK, created.Code)
	var row session.Session
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &row))
	// Authoritative model admission fails before any request message is durable.
	model := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+row.ID+"/agent/agent", `{"model":"missing/model","messages":[{"role":"user","content":"must not append"}]}`, "")
	assert.Equal(t, http.StatusUnprocessableEntity, model.Code, model.Body.String())
	h, err := sm.Handle(t.Context(), row.ID)
	require.NoError(t, err)
	before, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Empty(t, before.GetAllMessages())
	assert.Equal(t, "root", h.AgentName())
}

func TestLegacyAgentCommandChangesActiveAgentNotSessionIdentity(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := agent.New("root", "root prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithCommands(configtypes.Commands{"plan": {Agent: "worker"}}))
	worker := agent.New("worker", "worker prompt", agent.WithModel(sessionHTTPProvider{}))
	build := func(ctx context.Context, _ config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(root, worker)), runtime.WithSessionStore(store), runtime.WithWorkingDir(dir))
		if err != nil {
			return nil, err
		}
		return runtime.NewSessionRuntimeSupervisor(rt), nil
	}
	source := &memorySource{data: "version: '2'\nagents:\n  root:\n    model: test/model\n    instruction: test\n    commands:\n      plan:\n        agent: worker\n  worker:\n    model: test/model\n    instruction: test\n"}
	sm := NewSessionManager(t.Context(), config.Sources{"agent": source}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimeFactory(build))
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	srv := NewWithManager(sm, "")
	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{}`, "")
	require.Equal(t, http.StatusOK, created.Code)
	var row session.Session
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &row))
	response := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+row.ID+"/agent/agent", `{"messages":[{"role":"user","content":"/plan investigate"}]}`, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	h, err := sm.Handle(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, row.ID, h.ID())
	assert.Equal(t, "worker", h.AgentName())
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "root", snapshot.AttributesSnapshot()[sessionAgentAttribute])
	assert.Equal(t, "worker", snapshot.AgentName)
	count := 0
	for _, message := range snapshot.GetAllMessages() {
		if message.Message.Content == "investigate" {
			count++
		}
		assert.NotEqual(t, "/plan investigate", message.Message.Content)
	}
	assert.Equal(t, 1, count)
	canonical := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+row.ID+"/status", "", "")
	require.Equal(t, http.StatusOK, canonical.Code)
	assert.Contains(t, canonical.Body.String(), `"agent_name":"worker"`)
	stored, err := store.GetSession(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", stored.AttributesSnapshot()[sessionAgentAttribute])
	assert.Equal(t, "worker", stored.AgentName)
}
