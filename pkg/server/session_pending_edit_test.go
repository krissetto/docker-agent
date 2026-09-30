package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestPendingEditHTTPAuthLocalRemoteParity(t *testing.T) {
	store := session.NewInMemorySessionStore()
	tool := tools.Tool{Name: "danger", Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess("ok"), nil
	}}
	provider := &interactionProvider{streams: []chat.MessageStream{toolCallStream(), stopStream()}}
	srv, owner := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(provider), agent.WithToolSets(interactionToolSet{tool})))
	authenticated := NewWithManager(srv.sm, "secret")
	httpServer := httptest.NewServer(authenticated.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()), runtime.WithAuthToken("secret"))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	obs, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	active, err := handle.Submit(ctx, runtime.TurnInput{Content: "active"})
	require.NoError(t, err)
	waiting := false
	for !waiting {
		select {
		case event := <-obs.Events:
			_, waiting = event.Event.(*runtime.ToolCallConfirmationEvent)
		case <-ctx.Done():
			t.Fatal("interaction barrier not reached")
		}
	}
	queued, err := handle.Submit(ctx, runtime.TurnInput{Content: "old", RequestID: "original"})
	require.NoError(t, err)
	original := "old"
	edit := runtime.SessionEdit{Kind: runtime.SessionEditPendingMessage, PendingMessage: &runtime.PendingMessageEdit{TurnID: queued.TurnID, Content: "remote", ExpectedContent: &original}}
	body, err := json.Marshal(edit)
	require.NoError(t, err)
	for _, token := range []string{"", "wrong"} {
		rec := sessionRequest(t, authenticated, http.MethodPatch, "/api/v2/sessions/"+handle.ID(), string(body), token)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	snapshot, err := handle.Edit(ctx, edit)
	require.NoError(t, err)
	assert.Equal(t, "remote", snapshot.MessagesSnapshot()[snapshot.ItemCount()-1].Message.Message.Content)
	local, err := owner.Runtime().SessionByID(handle.ID())
	require.NoError(t, err)
	_, err = handle.Edit(ctx, edit)
	var typed *runtime.SessionError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, runtime.SessionErrorStale, typed.Kind)
	original = "remote"
	edit.PendingMessage.Content = "local"
	_, err = local.Edit(ctx, edit)
	require.NoError(t, err)
	observed := 0
	for observed < 2 {
		select {
		case envelope := <-obs.Events:
			if event, ok := envelope.Event.(*runtime.PendingUserMessageEditedEvent); ok {
				assert.Equal(t, queued.TurnID, event.TurnID)
				assert.Equal(t, queued.TurnID, envelope.TurnID)
				assert.Equal(t, []string{"remote", "local"}[observed], event.Message)
				observed++
			}
		case <-ctx.Done():
			t.Fatal("edited events did not arrive")
		}
	}
	stored, err := store.GetSession(ctx, handle.ID())
	require.NoError(t, err)
	assert.Equal(t, "local", stored.MessagesSnapshot()[stored.ItemCount()-1].Message.Message.Content)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"kind":"pending_message"}`, http.StatusBadRequest},
		{`{"kind":"pending_message","pending_message":{"turn_id":"missing","content":"new"}}`, http.StatusPreconditionFailed},
	} {
		rec := sessionRequest(t, authenticated, http.MethodPatch, "/api/v2/sessions/"+handle.ID(), tc.body, "secret")
		assert.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
	original = "local"
	edit.PendingMessage.Content = " "
	_, err = handle.Edit(ctx, edit)
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, runtime.SessionErrorInvalid, typed.Kind)
	_, err = handle.Cancel(ctx, queued.TurnID)
	require.NoError(t, err)
	edit.PendingMessage.Content = "late"
	_, err = handle.Edit(ctx, edit)
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, runtime.SessionErrorStale, typed.Kind)
	_, err = handle.Cancel(ctx, active.TurnID)
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(ctx, active.TurnID))
}

type pendingEditHTTPHandle struct {
	httpSession

	edits []runtime.SessionEdit
}

func (h *pendingEditHTTPHandle) Edit(_ context.Context, edit runtime.SessionEdit) (*session.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.edits = append(h.edits, edit)
	return h.snapshot.Clone(), nil
}

func TestPendingEditHTTPAttachedChildExactRouting(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv, store := newSessionHTTPServer(t, registry)
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAgentName("worker"))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	rootOwner := &pendingEditHTTPHandle{httpSession: httpSession{id: root.ID, agent: "root", snapshot: root}}
	childOwner := &pendingEditHTTPHandle{httpSession: httpSession{id: child.ID, agent: "worker", snapshot: child}}
	srv.sm.runtimeSessions.Store(root.ID, &activeRuntimes{handle: rootOwner, registry: registry})
	srv.sm.runtimeSessions.Store(child.ID, &activeRuntimes{handle: childOwner, registry: registry})
	authenticated := NewWithManager(srv.sm, "secret")
	body := `{"kind":"pending_message","pending_message":{"turn_id":"same-turn-id","content":"child only","expected_content":""}}`
	rec := sessionRequest(t, authenticated, http.MethodPatch, "/api/v2/sessions/child", body, "secret")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, rootOwner.edits)
	require.Len(t, childOwner.edits, 1)
	assert.Equal(t, "same-turn-id", childOwner.edits[0].PendingMessage.TurnID)
	assert.Equal(t, "child only", childOwner.edits[0].PendingMessage.Content)
	require.NotNil(t, childOwner.edits[0].PendingMessage.ExpectedContent)
	assert.Empty(t, *childOwner.edits[0].PendingMessage.ExpectedContent)
	var returned session.Session
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &returned))
	assert.Equal(t, child.ID, returned.ID)
	for _, token := range []string{"", "wrong"} {
		rec = sessionRequest(t, authenticated, http.MethodPatch, "/api/v2/sessions/child", body, token)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	rec = sessionRequest(t, authenticated, http.MethodPatch, "/api/v2/sessions/missing", body, "secret")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Len(t, childOwner.edits, 1, "authentication and wrong-owner lookup cannot dispatch an edit")
	assert.Empty(t, rootOwner.edits)
}
