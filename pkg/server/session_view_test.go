package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamdef"
)

func TestSessionViewRemoteConfirmedPrepareCommitAndResume(t *testing.T) {
	store := session.NewInMemorySessionStore()
	archived := session.New(session.WithID("archived"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	archived.CreatedAt = time.Unix(100, 0).UTC()
	archived.WorkingDir = "/synthetic/workspace"
	input := session.UserMessage("archived")
	input.TurnID, input.Pending, input.Accepted = "accepted", true, true
	archived.AddMessage(input)
	require.NoError(t, store.AddSession(t.Context(), archived))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(archived.WorkingDir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(owner.Runtime()))
	handler := NewWithManager(sm, "")
	server := httptest.NewServer(handler.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	prepared, err := transport.PrepareSessionView(t.Context(), archived.ID)
	require.NoError(t, err)
	info := prepared.Info()
	assert.Equal(t, archived.ID, info.SessionID)
	assert.Equal(t, archived.WorkingDir, info.WorkingDir)
	_, err = owner.Runtime().SessionByID(archived.ID)
	require.Error(t, err, "confirmed-info never publishes owner")
	info.Session.SetTitle("mutated")
	assert.NotEqual(t, "mutated", prepared.Info().Session.TitleSnapshot())
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Equal(t, archived.ID, committed.Metadata.SessionID)
	assert.True(t, committed.Status.Dormant)
	status, err := committed.SessionHandle.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Dormant)
	assert.Equal(t, 1, status.Pending)
	assert.Equal(t, archived.WorkingDir, committed.Info.WorkingDir)
	prepared.Abort()
	_, err = committed.SessionHandle.Edit(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditResume})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, committed.SessionHandle.AwaitTurn(ctx, "accepted"))
	status, err = committed.SessionHandle.Status(t.Context())
	require.NoError(t, err)
	assert.False(t, status.Dormant)
}

func TestSessionViewUnknownPrepareViewFailsBeforeColdHandle(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("cold"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	require.NoError(t, base.AddSession(t.Context(), root))
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	sm := NewSessionManager(t.Context(), nil, base, 0, nil, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "secret")
	for _, suffix := range []string{"?view=", "?view=unknown", "?view=prepare-info&view=unknown"} {
		response := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/cold"+suffix, "", "secret")
		assert.Equal(t, http.StatusBadRequest, response.Code)
	}
	assert.Zero(t, registry.createCount)
	unauthorized := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/cold?view=prepare-info", "", "")
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	assert.Zero(t, registry.createCount)
}

func TestSessionViewRemoteLegacyPrepareDoesNotCommitFallback(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		if !assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session_id": "old"})) {
			return
		}
	}))
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	_, err = transport.PrepareSessionView(t.Context(), "old")
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}

func TestSessionViewHTTPRejectsChangedConfirmationBeforePublication(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("confirmed"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(owner.Runtime()))
	srv := NewWithManager(sm, "secret")
	info, err := sm.ConfirmedSessionViewInfo(t.Context(), root.ID)
	require.NoError(t, err)
	confirmation := info.Confirmation()
	confirmation.WorkingDir = "/foreign"
	body, err := json.Marshal(runtime.SessionEdit{Kind: runtime.SessionEditOpenView, ViewConfirmation: &confirmation})
	require.NoError(t, err)
	response := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/confirmed?view=open-view", string(body), "secret")
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	_, err = owner.Runtime().SessionByID(root.ID)
	require.Error(t, err)
	assert.Zero(t, sm.runtimeSessions.Length())
	response = sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/confirmed?view=open-view", `{"kind":"open_view"}`, "secret")
	assert.Equal(t, http.StatusBadRequest, response.Code)
	confirmation = info.Confirmation()
	body, err = json.Marshal(runtime.SessionEdit{Kind: runtime.SessionEditOpenView, ViewConfirmation: &confirmation})
	require.NoError(t, err)
	response = sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/confirmed?view=open-view", string(body), "secret")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var ready api.SessionOpened[runtime.PreparedSessionViewInfo, runtime.SessionState]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &ready))
	assert.Equal(t, "open-view", ready.View)
	assert.Equal(t, root.ID, ready.Info.SessionID)
	assert.Equal(t, root.ID, ready.Metadata.SessionID)
	assert.True(t, ready.Status.Dormant)
	response = sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/confirmed", `{"kind":"open_view"}`, "secret")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var legacy session.Session
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &legacy))
	assert.Equal(t, root.ID, legacy.ID)
}

func TestHistoricalRemovedChildRemotePrepareCommitAndMutationDenial(t *testing.T) {
	base := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
	store := &historicalServerReadStore{InMemorySessionStore: base}
	const source = "team.yaml"
	root := session.New(session.WithID("history-root"), session.WithAgentName("root"), session.WithWorkingDir("/synthetic/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", sessionSourceAttribute: source}))
	require.NoError(t, store.AddSession(t.Context(), root))
	child := session.New(session.WithID("history-child"), session.WithAgentName("removed-worker"), session.WithParentID(root.ID), session.WithWorkingDir(root.WorkingDir), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "removed-worker", runtime.SessionParentAgentAttribute: "root", sessionSourceAttribute: source}), session.WithUserMessage("old child request"))
	child.AddMessage(session.NewAgentMessage("removed-worker", &chat.Message{Role: "assistant", Content: "old child answer"}))
	nodeID := subagent.NodeID("old-node")
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: nodeID, Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "removed-worker", Name: "old worker", State: subagent.NodeStopped}}
	require.NoError(t, store.AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	defs, err := teamdef.NewDefinitionSnapshot("old-team", []teamdef.AgentDefinition{
		{TeamID: "old-team", AgentID: "root-id", AgentKey: "root", Name: "root"},
		{TeamID: "old-team", AgentID: "removed-id", AgentKey: "removed-worker", Name: "removed-worker", Instruction: "Old instruction"},
	})
	require.NoError(t, err)
	entry, _ := defs.AgentDefinition("removed-worker")
	payloads := make([]session.TeamDefinitionPayload, 0, len(defs.Entries()))
	for _, payload := range defs.Entries() {
		payloads = append(payloads, session.TeamDefinitionPayload{Digest: payload.Digest, Kind: payload.Kind, Payload: payload.Payload})
	}
	_, err = store.StoreTeamRevision(t.Context(), session.TeamRevision{SessionID: child.ID, TeamID: defs.TeamID, TeamDigest: defs.TeamDigest, AgentDigest: entry.Digest, AgentKey: "removed-worker", AgentID: "removed-id", AgentName: "removed-worker", Origin: source}, payloads)
	require.NoError(t, err)
	trees := subagent.NewInMemoryStore()
	rootNode := subagent.Node{ID: subagent.SessionRootID(root.ID), Agent: "root", State: subagent.NodeIdle}
	require.NoError(t, trees.SaveTree(t.Context(), root.ID, subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode.ID, Nodes: []subagent.NodeSnapshot{{Node: rootNode, Children: []subagent.NodeSnapshot{{Node: record.Node}}}}}))
	rootAgent := agent.New("root", "current root", agent.WithModel(sessionHTTPProvider{}))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent)), runtime.WithSessionStore(store), runtime.WithSubagentStore(trees), runtime.WithWorkingDir(root.WorkingDir), runtime.WithSessionCompaction(false), runtime.WithModelStore(serverEmptyCatalogStore{}))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{source: config.NewBytesSource(source, []byte("agents: {root: {model: openai/gpt-4o}}\n"))}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimes(map[string]runtime.SessionRuntime{source: owner.Runtime()}))
	srv := NewWithManager(sm, "secret")
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithAuthToken("secret"), runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(source))
	require.NoError(t, err)
	prepared, err := transport.PrepareSessionView(t.Context(), child.ID)
	require.NoError(t, err)
	info := prepared.Info()
	require.True(t, info.Historical)
	metadataChild := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/history-child?view=prepare-info&projection=metadata", "", "secret")
	require.Equal(t, http.StatusOK, metadataChild.Code, metadataChild.Body.String())
	var metadataChildBody map[string]any
	require.NoError(t, json.Unmarshal(metadataChild.Body.Bytes(), &metadataChildBody))
	metadataChildInfo := metadataChildBody["info"].(map[string]any)
	require.Equal(t, "metadata", metadataChildBody["projection"])
	require.Equal(t, string(record.Node.ID), metadataChildInfo["attach"].(map[string]any)["NodeID"])
	assert.NotContains(t, metadataChildInfo["attach"].(map[string]any), "Session")
	assert.Equal(t, root.ID, metadataChildInfo["binding"].(map[string]any)["ParentSessionID"])
	require.True(t, info.Historical)
	require.Equal(t, "removed-worker", info.ActiveAgentName)
	require.NotNil(t, info.Attach)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	require.True(t, committed.Metadata.Historical)
	require.False(t, committed.Metadata.Capabilities.ExecutionAvailable)
	snapshot, err := committed.SessionHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "old child answer", snapshot.GetLastAssistantMessageContent())
	_, err = committed.SessionHandle.Submit(t.Context(), runtime.TurnInput{Content: "must not execute"})
	var historical *runtime.SessionError
	require.ErrorAs(t, err, &historical)
	require.Equal(t, runtime.SessionErrorHistorical, historical.Kind)
	response := sessionRequest(t, srv, http.MethodPost, api.SessionAPIPath+"/history-child/messages", `{"content":"must not execute"}`, "secret")
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	var payload map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Equal(t, string(runtime.SessionErrorHistorical), payload["error"])
	_, loaded := sm.runtimeSessions.Load(child.ID)
	require.False(t, loaded, "historical view must not be registered as a live executable owner")
	store.mu.Lock()
	own, full := append([]string(nil), store.own...), append([]string(nil), store.full...)
	store.mu.Unlock()
	require.NotEmpty(t, own)
	for _, loadedID := range own {
		require.Equal(t, child.ID, loadedID, "server attach and later requests may reload selected history, never siblings")
	}
	require.Empty(t, full, "metadata-first server handle must never recursively hydrate root/sibling transcripts")
}

type historicalServerReadStore struct {
	*session.InMemorySessionStore
	mu   sync.Mutex
	own  []string
	full []string
}

func (s *historicalServerReadStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	s.full = append(s.full, id)
	s.mu.Unlock()
	return s.InMemorySessionStore.GetSession(ctx, id)
}
func (s *historicalServerReadStore) GetSessionMetadata(ctx context.Context, id string) (*session.Session, error) {
	return s.InMemorySessionStore.GetSessionMetadata(ctx, id)
}
func (s *historicalServerReadStore) GetSessionOwnHistory(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	s.own = append(s.own, id)
	s.mu.Unlock()
	return s.InMemorySessionStore.GetSessionOwnHistory(ctx, id)
}

type serverEmptyCatalogStore struct{ runtime.ModelStore }

func (serverEmptyCatalogStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	return nil, nil
}
func (serverEmptyCatalogStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	return &modelsdev.Database{}, nil
}

func TestSessionViewMetadataProjectionBoundsPrepareWithoutChangingDefault(t *testing.T) {
	store := session.NewInMemorySessionStore()
	archived := session.New(
		session.WithID("large-view"),
		session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", "custom": "preserved"}),
		session.WithUserMessage(strings.Repeat("large transcript ", 350_000)),
	)
	archived.WorkingDir = "/synthetic/workspace"
	require.NoError(t, store.AddSession(t.Context(), archived))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(archived.WorkingDir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(owner.Runtime()))
	srv := NewWithManager(sm, "secret")

	legacy := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/large-view?view=prepare-info", "", "secret")
	require.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
	require.Greater(t, legacy.Body.Len(), 5*1024*1024)
	var legacyBody map[string]any
	require.NoError(t, json.Unmarshal(legacy.Body.Bytes(), &legacyBody))
	assert.NotContains(t, legacyBody, "projection", "legacy clients retain the old envelope")
	legacyInfo := legacyBody["info"].(map[string]any)
	legacySession := legacyInfo["session"].(map[string]any)
	assert.Contains(t, legacySession, "messages")

	metadata := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/large-view?view=prepare-info&projection=metadata", "", "secret")
	require.Equal(t, http.StatusOK, metadata.Code, metadata.Body.String())
	require.Less(t, metadata.Body.Len(), 4*1024*1024, "metadata response must fit the native 4 MiB response bound and not scale with transcript bytes")
	var metadataBody map[string]any
	require.NoError(t, json.Unmarshal(metadata.Body.Bytes(), &metadataBody))
	require.Equal(t, api.SessionViewMetadataProjection, metadataBody["projection"])
	metadataInfo := metadataBody["info"].(map[string]any)
	metadataSession := metadataInfo["session"].(map[string]any)
	assert.Equal(t, "large-view", metadataSession["id"])
	assert.Equal(t, archived.WorkingDir, metadataSession["working_dir"])
	assert.Equal(t, "root", metadataSession["attributes"].(map[string]any)[runtime.SessionAgentAttribute])
	assert.Equal(t, "preserved", metadataSession["attributes"].(map[string]any)["custom"])
	assert.NotContains(t, metadataSession, "messages")
	assert.NotContains(t, metadataSession, "subagent_tree")
	assert.NotContains(t, metadata.Body.String(), "large transcript")
}

func TestSessionViewMetadataProjectionOpenAndConfirmationSemantics(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("metadata-open"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	root.WorkingDir = "/synthetic/workspace"
	root.AddMessage(session.UserMessage(strings.Repeat("open transcript ", 10_000)))
	require.NoError(t, store.AddSession(t.Context(), root))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(root.WorkingDir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(owner.Runtime()))
	srv := NewWithManager(sm, "secret")

	prepared := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/metadata-open?view=prepare-info&projection=metadata", "", "secret")
	require.Equal(t, http.StatusOK, prepared.Code, prepared.Body.String())
	var response struct {
		Projection string                          `json:"projection"`
		Info       runtime.PreparedSessionViewInfo `json:"info"`
	}
	require.NoError(t, json.Unmarshal(prepared.Body.Bytes(), &response))
	require.Equal(t, api.SessionViewMetadataProjection, response.Projection)
	require.NotNil(t, response.Info.Session)
	assert.Equal(t, root.ID, response.Info.Session.ID)
	assert.Nil(t, response.Info.Session.Messages, "metadata projection must not hydrate transcript into its response DTO")
	confirmation := response.Info.Confirmation()
	assert.Equal(t, root.ID, confirmation.SessionID)
	assert.Equal(t, response.Info.RootSessionID, confirmation.RootSessionID)
	assert.Equal(t, response.Info.Binding, confirmation.Binding)

	badConfirmation := confirmation
	badConfirmation.WorkingDir = "/foreign"
	body, err := json.Marshal(runtime.SessionEdit{Kind: runtime.SessionEditOpenView, ViewConfirmation: &badConfirmation})
	require.NoError(t, err)
	mismatch := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/metadata-open?view=open-view&projection=metadata", string(body), "secret")
	require.Equal(t, http.StatusConflict, mismatch.Code, mismatch.Body.String())
	_, published := owner.Runtime().SessionByID(root.ID)
	require.Error(t, published, "confirmation mismatch must not publish a session owner")

	body, err = json.Marshal(runtime.SessionEdit{Kind: runtime.SessionEditOpenView, ViewConfirmation: &confirmation})
	require.NoError(t, err)
	opened := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/metadata-open?view=open-view&projection=metadata", string(body), "secret")
	require.Equal(t, http.StatusOK, opened.Code, opened.Body.String())
	var openedBody map[string]any
	require.NoError(t, json.Unmarshal(opened.Body.Bytes(), &openedBody))
	assert.Equal(t, api.SessionViewMetadataProjection, openedBody["projection"])
	assert.Contains(t, openedBody, "metadata")
	assert.Contains(t, openedBody, "status")
	openedSession := openedBody["info"].(map[string]any)["session"].(map[string]any)
	assert.NotContains(t, openedSession, "messages")
	assert.NotContains(t, openedSession, "subagent_tree")

	legacyOpen := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/metadata-open?view=open-view", string(body), "secret")
	require.Equal(t, http.StatusOK, legacyOpen.Code, legacyOpen.Body.String())
	var legacyOpenBody map[string]any
	require.NoError(t, json.Unmarshal(legacyOpen.Body.Bytes(), &legacyOpenBody))
	assert.NotContains(t, legacyOpenBody, "projection")
	legacyOpenSession := legacyOpenBody["info"].(map[string]any)["session"].(map[string]any)
	assert.Contains(t, legacyOpenSession, "messages")
}

func TestSessionViewMetadataProjectionRejectsUnsupportedOptionBeforePrepare(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("cold-projection"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	require.NoError(t, base.AddSession(t.Context(), root))
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	sm := NewSessionManager(t.Context(), nil, base, 0, nil, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "secret")
	for _, path := range []string{
		"/api/v2/sessions/cold-projection?view=prepare-info&projection=unknown",
		"/api/v2/sessions/cold-projection?view=prepare-info&projection=metadata&projection=metadata",
		"/api/v2/sessions/cold-projection?projection=metadata",
	} {
		response := sessionRequest(t, srv, http.MethodGet, path, "", "secret")
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
	assert.Zero(t, registry.createCount)
	patch := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/cold-projection?projection=metadata", `{"kind":"open_view"}`, "secret")
	assert.Equal(t, http.StatusBadRequest, patch.Code, patch.Body.String())
	assert.Zero(t, registry.createCount)
}

func TestMetadataPreparedViewInfoSizeIgnoresSessionAndAttachHistory(t *testing.T) {
	selected := session.New(session.WithID("metadata-size"), session.WithAttributes(map[string]string{sessionSourceAttribute: "source"}))
	attachSession := selected.Clone()
	info := runtime.PreparedSessionViewInfo{
		ActiveAgentName: "worker",
		SessionID:       selected.ID,
		RootSessionID:   "metadata-root",
		RootWorkingDir:  "/workspace",
		RootBinding:     runtime.SessionBinding{AgentName: "root"},
		Session:         selected,
		Binding:         runtime.SessionBinding{AgentName: "worker", ParentSessionID: "metadata-root"},
		WorkingDir:      "/workspace",
		Attach:          &runtime.SubagentAttachInfo{NodeID: subagent.NodeID("metadata-node"), Agent: "worker", Name: "Worker", Session: attachSession, ParentSessionID: "metadata-root", ParentAgent: "root"},
	}
	encode := func() []byte {
		data, err := json.Marshal(struct {
			Version    int                             `json:"version"`
			View       string                          `json:"view"`
			Projection string                          `json:"projection"`
			Info       metadataPreparedSessionViewInfo `json:"info"`
		}{Version: api.SessionAPIVersion, View: "prepare-info", Projection: api.SessionViewMetadataProjection, Info: metadataPreparedViewInfo(info)})
		require.NoError(t, err)
		return data
	}
	before := encode()
	largeHistory := strings.Repeat("history must not affect response size ", 100_000)
	selected.AddMessage(session.UserMessage(largeHistory))
	attachSession.AddMessage(session.UserMessage(largeHistory))
	tree := &subagent.Snapshot{Version: subagent.SnapshotVersion, Root: subagent.NodeID("tree-root")}
	selected.SetSubagentTree(tree)
	attachSession.SetSubagentTree(tree)
	after := encode()
	assert.Equal(t, before, after, "selected and attached transcript/topology are not serialized")
	assert.NotContains(t, string(after), "history must not affect response size")
}
