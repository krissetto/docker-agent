package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCanonicalHTTPRemoteExactTurnWaitCancelAndResolution(t *testing.T) {
	transport, closeFn := realInteractionHTTP(t, 10)
	defer closeFn()
	handle, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	obs, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	active, err := handle.Submit(ctx, runtime.TurnInput{Content: "active", RequestID: "active-request"})
	require.NoError(t, err)
	var interaction string
	for interaction == "" {
		select {
		case event := <-obs.Events:
			if _, ok := event.Event.(*runtime.ToolCallConfirmationEvent); ok {
				interaction = event.InteractionID
			}
		case <-ctx.Done():
			t.Fatal("interaction did not arrive")
		}
	}
	queued, err := handle.Submit(ctx, runtime.TurnInput{Content: "queued", RequestID: "queued-request"})
	require.NoError(t, err)
	result, err := handle.Cancel(ctx, queued.TurnID)
	require.NoError(t, err)
	assert.Equal(t, queued.TurnID, result.TurnID)
	require.NoError(t, handle.AwaitTurn(ctx, queued.TurnID))
	status, err := handle.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, active.TurnID, status.TurnID, "canceling queued work must not affect active work")
	waitCtx, stopWait := context.WithCancel(ctx)
	stopWait()
	require.ErrorIs(t, handle.AwaitTurn(waitCtx, active.TurnID), context.Canceled)
	_, err = handle.Cancel(ctx, active.TurnID)
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(ctx, active.TurnID))
	for {
		select {
		case event := <-obs.Events:
			if resolved, ok := event.Event.(*runtime.InteractionResolvedEvent); ok {
				assert.Equal(t, interaction, event.InteractionID)
				assert.Equal(t, interaction, resolved.InteractionID)
				assert.Equal(t, "canceled", string(resolved.Reason))
				return
			}
		case <-ctx.Done():
			t.Fatal("interaction resolution did not arrive")
		}
	}
}

func TestCanonicalHTTPRemoteDedupAndEdit(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input := runtime.TurnInput{Content: "once", RequestID: "request"}
	first, err := handle.Submit(ctx, input)
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(ctx, first.TurnID))
	again, err := handle.Submit(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, first.TurnID, again.TurnID)
	input.Content = "different"
	_, err = handle.Submit(ctx, input)
	var typed *runtime.SessionError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, runtime.SessionErrorConflict, typed.Kind)
	require.ErrorAs(t, handle.AwaitTurn(ctx, "unknown"), &typed)
	assert.Equal(t, runtime.SessionErrorNotFound, typed.Kind)
	updated, err := handle.Edit(ctx, runtime.SessionEdit{Kind: runtime.SessionEditTitle, Title: "edited"})
	require.NoError(t, err)
	assert.Equal(t, "edited", updated.TitleSnapshot())
	stored, err := store.GetSession(ctx, handle.ID())
	require.NoError(t, err)
	assert.Equal(t, "edited", stored.TitleSnapshot())
}

func TestCanonicalSnapshotChunkFraming(t *testing.T) {
	snapshot := sessionSnapshotDTO{Session: session.New(session.WithTitle(strings.Repeat("x", 3*sessionSnapshotChunkBytes))), Cursor: 9}
	var frames []sessionStreamMessage
	require.NoError(t, writeSessionSnapshot(snapshot, func(frame sessionStreamMessage) error {
		frames = append(frames, frame)
		return nil
	}))
	require.Greater(t, len(frames), 3)
	assert.Equal(t, "snapshot_begin", frames[0].Type)
	assert.Equal(t, "snapshot_end", frames[len(frames)-1].Type)
	var data []byte
	for _, frame := range frames[1 : len(frames)-1] {
		assert.Equal(t, "snapshot_chunk", frame.Type)
		assert.Equal(t, snapshot.Cursor, frame.Cursor)
		assert.LessOrEqual(t, len(frame.Chunk), sessionSnapshotChunkBytes)
		data = append(data, frame.Chunk...)
	}
	var decoded sessionSnapshotDTO
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, snapshot.Session.TitleSnapshot(), decoded.Session.TitleSnapshot())
	assert.Equal(t, snapshot.Cursor, decoded.Cursor)
}

func TestCanonicalHTTPWaitUnknownTurnReturnsTypedNotFound(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	created := sessionRequest(t, srv, http.MethodPost, "/api/sessions", `{"agent_name":"root"}`, "")
	require.Equal(t, http.StatusCreated, created.Code)
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))
	response := sessionRequest(t, srv, http.MethodPost, "/api/sessions/"+metadata.SessionID+"/turns/unknown/wait", "", "")
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Contains(t, response.Body.String(), `"error":"not_found"`)
}

func TestCanonicalHTTPRemoteLargeSnapshotRoundTrip(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, owner := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	original := session.New(session.WithTitle(strings.Repeat("history", 700000)))
	local, err := owner.Runtime().CreateSession(t.Context(), original, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID(local.ID())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observation, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	assert.Equal(t, original.TitleSnapshot(), observation.Primary().Session.TitleSnapshot())
	assert.Equal(t, local.ID(), observation.Primary().Status.SessionID)
	fetched, err := handle.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, original.TitleSnapshot(), fetched.TitleSnapshot(), "JSON snapshots also preserve history beyond the old 4MiB limit")
}

func TestCanonicalHTTPRemotePersistenceFailureDetailWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	e := echo.New()
	e.POST("/api/sessions/s/messages", func(echo.Context) error {
		requests.Add(1)
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorPersistence, SessionID: "s", Operation: "submit", Detail: "session writes are blocked: schema mismatch"})
	})
	httpServer := httptest.NewServer(e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	submission, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "blocked", RequestID: "blocked-request"})
	var typed *runtime.SessionError
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, runtime.SessionErrorPersistence, typed.Kind)
	assert.Equal(t, "session writes are blocked: schema mismatch", typed.Detail)
	assert.Empty(t, submission.TurnID)
	assert.Equal(t, int32(1), requests.Load(), "transport must not retry a rejected submission")
	var httpErr *echo.HTTPError
	require.ErrorAs(t, sessionHTTPError(typed), &httpErr)
	assert.Equal(t, http.StatusServiceUnavailable, httpErr.Code)
}
