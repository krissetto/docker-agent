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

func TestPendingRecallHTTPAuthLocalRemoteParity(t *testing.T) {
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
	queued, err := handle.Submit(ctx, runtime.TurnInput{Content: "remove", RequestID: "withdrawn"})
	require.NoError(t, err)
	keep, err := handle.Submit(ctx, runtime.TurnInput{Content: "keep", RequestID: "retained"})
	require.NoError(t, err)
	body := `{"turn_id":"` + queued.TurnID + `","pending_only":true}`
	for _, token := range []string{"", "wrong"} {
		rec := sessionRequest(t, authenticated, http.MethodPost, "/api/v2/sessions/"+handle.ID()+"/cancel", body, token)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	canceler := handle.(runtime.PendingMessageCanceler)
	for _, id := range []string{"", "unknown", active.TurnID} {
		removed, err := canceler.CancelPendingMessage(ctx, id)
		require.NoError(t, err)
		assert.False(t, removed)
	}
	removed, err := canceler.CancelPendingMessage(ctx, queued.TurnID)
	require.NoError(t, err)
	require.True(t, removed)
	require.NoError(t, handle.AwaitTurn(ctx, queued.TurnID))
	removed, err = canceler.CancelPendingMessage(ctx, queued.TurnID)
	require.NoError(t, err)
	assert.False(t, removed)
	status, err := handle.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, active.TurnID, status.TurnID)
	assert.Equal(t, runtime.SessionStateRunning, status.State)
	assert.Equal(t, 1, status.Pending)
	stored, err := store.GetSession(ctx, handle.ID())
	require.NoError(t, err)
	for _, item := range stored.MessagesSnapshot() {
		if item.Message != nil {
			assert.NotEqual(t, queued.TurnID, item.Message.TurnID)
		}
	}
	_, err = handle.Submit(ctx, runtime.TurnInput{Content: "remove", RequestID: "withdrawn"})
	var typed *runtime.SessionError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, runtime.SessionErrorConflict, typed.Kind, "withdrawal tombstone blocks re-admission")
	local, err := owner.Runtime().SessionByID(handle.ID())
	require.NoError(t, err)
	removed, err = local.(runtime.PendingMessageCanceler).CancelPendingMessage(ctx, keep.TurnID)
	require.NoError(t, err)
	require.True(t, removed)
	observed := map[string]bool{}
	for len(observed) < 2 {
		select {
		case envelope := <-obs.Events:
			if event, ok := envelope.Event.(*runtime.PendingUserMessageCanceledEvent); ok {
				assert.Equal(t, handle.ID(), event.SessionID)
				assert.Equal(t, handle.ID(), envelope.SessionID)
				assert.Equal(t, event.TurnID, envelope.TurnID)
				assert.GreaterOrEqual(t, event.SessionPosition, 0)
				assert.Equal(t, -1, envelope.TranscriptPosition, "withdrawal retains the canonical non-transcript envelope")
				observed[event.TurnID] = true
			}
		case <-ctx.Done():
			t.Fatal("withdrawal projection did not arrive")
		}
	}
	assert.True(t, observed[queued.TurnID])
	assert.True(t, observed[keep.TurnID])
	_, err = handle.Cancel(ctx, active.TurnID)
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(ctx, active.TurnID))
	removed, err = canceler.CancelPendingMessage(ctx, active.TurnID)
	require.NoError(t, err)
	assert.False(t, removed, "settled input cannot be withdrawn")
}

type pendingRecallHTTPHandle struct {
	httpSession
	recalled  []string
	recallErr error
}

func (h *pendingRecallHTTPHandle) CancelPendingMessage(_ context.Context, turnID string) (bool, error) {
	h.recalled = append(h.recalled, turnID)
	return h.recallErr == nil, h.recallErr
}

func TestPendingRecallHTTPAttachedChildExactRoutingAndErrors(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv, store := newSessionHTTPServer(t, registry)
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAgentName("worker"))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	rootOwner := &pendingRecallHTTPHandle{httpSession: httpSession{id: root.ID, agent: "root", snapshot: root}}
	childOwner := &pendingRecallHTTPHandle{httpSession: httpSession{id: child.ID, agent: "worker", snapshot: child}}
	srv.sm.runtimeSessions.Store(root.ID, &activeRuntimes{handle: rootOwner, registry: registry})
	srv.sm.runtimeSessions.Store(child.ID, &activeRuntimes{handle: childOwner, registry: registry})
	authenticated := NewWithManager(srv.sm, "secret")
	body := `{"turn_id":"same-turn-id","pending_only":true}`
	rec := sessionRequest(t, authenticated, http.MethodPost, "/api/v2/sessions/child/cancel", body, "secret")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, rootOwner.recalled)
	assert.Equal(t, []string{"same-turn-id"}, childOwner.recalled)
	assert.Empty(t, childOwner.cancels, "pending-only dispatch cannot fall back to active cancellation")
	var result runtime.CancelResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, runtime.CancelResult{SessionID: child.ID, TurnID: "same-turn-id", Outcome: runtime.CancelAccepted}, result)
	for _, token := range []string{"", "wrong"} {
		rec = sessionRequest(t, authenticated, http.MethodPost, "/api/v2/sessions/child/cancel", body, token)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	rec = sessionRequest(t, authenticated, http.MethodPost, "/api/v2/sessions/missing/cancel", body, "secret")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Len(t, childOwner.recalled, 1)
	childOwner.recallErr = &runtime.SessionError{Kind: runtime.SessionErrorPersistence, SessionID: child.ID, Operation: "cancel_pending_message"}
	rec = sessionRequest(t, authenticated, http.MethodPost, "/api/v2/sessions/child/cancel", body, "secret")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, childOwner.cancels)
	srv.sm.runtimeSessions.Store(child.ID, &activeRuntimes{handle: &childOwner.httpSession, registry: registry})
	rec = sessionRequest(t, authenticated, http.MethodPost, "/api/v2/sessions/child/cancel", body, "secret")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Empty(t, childOwner.cancels, "unsupported owner must fail closed")
}
