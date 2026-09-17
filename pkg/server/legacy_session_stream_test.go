package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// Decode exactly the old raw data/type protocol, with no canonical envelope.
func legacyStreamFrames(t *testing.T, body string) ([]map[string]json.RawMessage, []uint64) {
	t.Helper()
	var frames []map[string]json.RawMessage
	var ids []uint64
	var id uint64
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			var err error
			id, err = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
			require.NoError(t, err)
		}
		if strings.HasPrefix(line, "data: ") {
			var event map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
			require.NotEmpty(t, event["type"])
			require.NotContains(t, event, "envelope")
			frames = append(frames, event)
			ids = append(ids, id)
			id = 0
		}
		require.False(t, strings.HasPrefix(line, "event:"), "legacy streams have no named SSE events")
	}
	return frames, ids
}

func legacyStreamContext(t *testing.T, ctx context.Context) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil), recorder)
	return c, recorder
}

func TestLegacyStreamRawRunOrderingAndExactTurn(t *testing.T) {
	handle := &legacyLifecycleHandle{httpSession: &httpSession{id: "session", agent: "root"}}
	confirmation := &runtime.ToolCallConfirmationEvent{Type: "tool_call_confirmation", SessionID: handle.id, RequestID: "confirm"}
	maximum := runtime.MaxIterationsReachedForSession(3, handle.id, "max")
	elicitation := &runtime.ElicitationRequestEvent{Type: "elicitation_request", SessionID: handle.id, ElicitationID: "ask", RequestID: "elicit"}
	events := []runtime.Event{runtime.StreamStarted(handle.id, "root"), confirmation, maximum, elicitation, runtime.StreamStopped(handle.id, "root", "completed")}
	var canceled atomic.Int32
	observation := runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithTitle("existing"))}}, Cancel: func() { canceled.Add(1) }}
	observation.Replay = append(observation.Replay, runtime.SessionEvent{SessionID: handle.id, TurnID: "other", Event: runtime.StreamStopped(handle.id, "root", "completed")})
	for i, event := range events {
		observation.Replay = append(observation.Replay, runtime.SessionEvent{SessionID: handle.id, TurnID: "own", Sequence: uint64(i + 1), Event: event})
	}
	observation.Replay = append(observation.Replay, runtime.SessionEvent{SessionID: handle.id, TurnID: "own", Event: runtime.Error("must not follow terminal")})
	c, recorder := legacyStreamContext(t, t.Context())
	require.NoError(t, (&Server{}).legacyRunStream(c, handle, observation, "own", true))
	frames, ids := legacyStreamFrames(t, recorder.Body.String())
	require.Len(t, frames, len(events)+1)
	assert.JSONEq(t, `"session_title"`, string(frames[0]["type"]))
	for i, event := range events {
		data, err := json.Marshal(event)
		require.NoError(t, err)
		actual, err := json.Marshal(frames[i+1])
		require.NoError(t, err)
		assert.JSONEq(t, string(data), string(actual))
	}
	assert.Equal(t, make([]uint64, len(frames)), ids)
	assert.EqualValues(t, 1, canceled.Load())
	assert.Empty(t, handle.cancels)
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
}

func TestLegacyStreamCursorCompatibilityAndInteractionReseed(t *testing.T) {
	for _, tc := range []struct {
		query, header string
		want          uint64
	}{
		{"", "", 0}, {"?since=7", "9", 7}, {"", "9", 9}, {"?since=invalid", "9", 0}, {"?since=-1", "", 0}, {"?since=18446744073709551616", "9", 0}, {"?since=", "9", 9},
	} {
		t.Run(tc.query+"/"+tc.header, func(t *testing.T) {
			handle := &httpSession{id: "session"}
			prompt := runtime.MaxIterationsReachedForSession(3, handle.id, "prompt")
			handle.attach = runtime.Observation{
				Initial: []runtime.SessionSnapshot{{Interactions: []runtime.InteractionSnapshot{{SessionID: handle.id, InteractionID: "prompt", Event: prompt}}}},
				Replay:  []runtime.SessionEvent{{SessionID: handle.id, TurnID: "other", Sequence: 10, Event: runtime.StreamStopped(handle.id, "root", "completed")}},
				Events:  make(chan runtime.SessionEvent, 1),
			}
			// A deletion is an owner terminal, unlike the completed turn above.
			live := make(chan runtime.SessionEvent, 1)
			live <- runtime.SessionEvent{SessionID: handle.id, Sequence: 11, Event: runtime.StreamStopped(handle.id, "root", "deleted")}
			handle.attach.Events = live
			srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{handle.id: handle}})
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/"+tc.query, nil)
			request.Header.Set("Last-Event-ID", tc.header)
			recorder := httptest.NewRecorder()
			c := srv.e.NewContext(request, recorder)
			c.SetParamNames("id")
			c.SetParamValues(handle.id)
			require.NoError(t, srv.legacySessionEvents(c))
			require.Len(t, handle.attachOptions, 1)
			require.NotNil(t, handle.attachOptions[0].Since)
			assert.Equal(t, tc.want, *handle.attachOptions[0].Since)
			frames, ids := legacyStreamFrames(t, recorder.Body.String())
			require.Len(t, frames, 3)
			assert.Equal(t, []uint64{10, 0, 11}, ids)
			assert.JSONEq(t, `"stream_stopped"`, string(frames[0]["type"]))
			assert.JSONEq(t, `"max_iterations_reached"`, string(frames[1]["type"]))
			assert.JSONEq(t, `"session_exited"`, string(frames[2]["type"]))
		})
	}
}

func TestLegacyStreamReplayPromptIsNotReseededTwice(t *testing.T) {
	handle := &httpSession{id: "session"}
	prompt := runtime.MaxIterationsReachedForSession(3, handle.id, "prompt")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	live := make(chan runtime.SessionEvent, 1)
	live <- runtime.SessionEvent{SessionID: handle.id, Sequence: 3, Event: runtime.StreamStopped(handle.id, "root", "deleted")}
	obs := runtime.Observation{
		Initial: []runtime.SessionSnapshot{{Interactions: []runtime.InteractionSnapshot{{SessionID: handle.id, InteractionID: "prompt", Event: prompt}}}},
		Replay:  []runtime.SessionEvent{{SessionID: handle.id, Sequence: 2, InteractionID: "prompt", Event: prompt}}, Events: live,
	}
	c, rec := legacyStreamContext(t, ctx)
	require.NoError(t, (&Server{}).legacyStream(c, handle, obs, "", false))
	frames, _ := legacyStreamFrames(t, rec.Body.String())
	require.Len(t, frames, 2)
}

func TestLegacyStreamGapIsBarrierAndRunError(t *testing.T) {
	for _, turnID := range []string{"", "own"} {
		t.Run(turnID, func(t *testing.T) {
			handle := &httpSession{id: "session"}
			var canceled atomic.Int32
			obs := runtime.Observation{Replay: []runtime.SessionEvent{{Gap: true, FirstAvailable: 20}, {TurnID: turnID, Event: runtime.Error("stale tail")}}, Events: make(chan runtime.SessionEvent), Cancel: func() { canceled.Add(1) }}
			c, rec := legacyStreamContext(t, t.Context())
			require.NoError(t, (&Server{}).legacyStream(c, handle, obs, turnID, turnID != ""))
			frames, ids := legacyStreamFrames(t, rec.Body.String())
			assert.JSONEq(t, `"gap"`, string(frames[0]["type"]))
			assert.NotContains(t, rec.Body.String(), "stale tail")
			assert.EqualValues(t, 1, canceled.Load())
			assert.Empty(t, handle.cancels)
			assert.Equal(t, make([]uint64, len(frames)), ids)
			if turnID != "" {
				require.Len(t, frames, 2)
				assert.JSONEq(t, `"observation_gap"`, string(frames[1]["code"]))
			} else {
				require.Len(t, frames, 1)
			}
		})
	}
}

type legacyLifecycleHandle struct {
	*httpSession

	observeErr error
	awaited    []string
	await      func(context.Context, string) error
	cancelErr  error
}

func (h *legacyLifecycleHandle) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	if h.observeErr != nil {
		return runtime.Observation{}, h.observeErr
	}
	return h.httpSession.Observe(ctx, options)
}

func (h *legacyLifecycleHandle) Cancel(ctx context.Context, turnID string) (runtime.CancelResult, error) {
	result, err := h.httpSession.Cancel(ctx, turnID)
	return result, errors.Join(err, h.cancelErr)
}

func (h *legacyLifecycleHandle) AwaitTurn(ctx context.Context, turnID string) error {
	if h.await != nil {
		return h.await(ctx, turnID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.awaited = append(h.awaited, turnID)
	return nil
}

func TestLegacyStreamEOFDoesNotInventSessionExit(t *testing.T) {
	for _, kind := range []runtime.SessionErrorKind{"", runtime.SessionErrorStopped, runtime.SessionErrorClosed, runtime.SessionErrorNotFound, runtime.SessionErrorUnsupported} {
		t.Run(string(kind), func(t *testing.T) {
			handle := &legacyLifecycleHandle{httpSession: &httpSession{id: "session"}}
			if kind != "" {
				handle.observeErr = &runtime.SessionError{Kind: kind}
			}
			live := make(chan runtime.SessionEvent)
			close(live)
			c, rec := legacyStreamContext(t, t.Context())
			require.NoError(t, (&Server{}).legacyStream(c, handle, runtime.Observation{Events: live}, "", false))
			terminal := kind == runtime.SessionErrorStopped || kind == runtime.SessionErrorClosed || kind == runtime.SessionErrorNotFound
			assert.Equal(t, terminal, strings.Contains(rec.Body.String(), "session_exited"))
			if kind == "" {
				assert.Equal(t, 1, handle.observationCancels, "probe must detach")
			}
		})
	}
}

func TestLegacyStreamDisconnectOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, turn string
		owned      bool
	}{{"get", "", false}, {"borrowed run", "own", false}, {"admitting run", "own", true}} {
		t.Run(tc.name, func(t *testing.T) {
			handle := &legacyLifecycleHandle{httpSession: &httpSession{id: "session"}}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var detached atomic.Int32
			c, _ := legacyStreamContext(t, ctx)
			require.NoError(t, (&Server{}).legacyStream(c, handle, runtime.Observation{Events: make(chan runtime.SessionEvent), Cancel: func() { detached.Add(1) }}, tc.turn, tc.owned))
			assert.EqualValues(t, 1, detached.Load())
			if tc.owned {
				assert.Equal(t, []string{"own"}, handle.cancels)
				assert.Equal(t, []string{"own"}, handle.awaited)
			} else {
				assert.Empty(t, handle.cancels)
				assert.Empty(t, handle.awaited)
			}
		})
	}
}

func TestLegacyStreamObservationFailureProjectsRawError(t *testing.T) {
	handle := &httpSession{id: "session"}
	errorsCh := make(chan error, 1)
	errorsCh <- errors.New("connection lost")
	c, rec := legacyStreamContext(t, t.Context())
	require.NoError(t, (&Server{}).legacyRunStream(c, handle, runtime.Observation{Events: make(chan runtime.SessionEvent), Errors: errorsCh}, "own", true))
	frames, _ := legacyStreamFrames(t, rec.Body.String())
	require.Len(t, frames, 1)
	assert.JSONEq(t, `"observation_failed"`, string(frames[0]["code"]))
	assert.Empty(t, handle.cancels)
}

func TestLegacyStreamRealSharedV2ObserverAndDelete(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, owner := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	srv.heartbeatInterval = time.Millisecond
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	// Register an independent test route to avoid coupling adapter proof to
	// the concurrent legacy request-admission implementation.
	srv.e.GET("/legacy-test/:id", srv.legacySessionEvents)
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(handle.ID())
	require.NoError(t, err)
	v2, err := remote.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer v2.Cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/legacy-test/"+handle.ID(), http.NoBody)
	require.NoError(t, err)
	response, err := httpServer.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, ": ping\n", line)
	submission, err := handle.Submit(ctx, runtime.TurnInput{Content: "shared"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(ctx, submission.TurnID))
	var v2Stopped bool
	for !v2Stopped {
		select {
		case envelope := <-v2.Events:
			_, stopped := envelope.Event.(*runtime.StreamStoppedEvent)
			v2Stopped = stopped && envelope.TurnID == submission.TurnID
		case <-ctx.Done():
			t.Fatal("v2 did not receive shared turn")
		}
	}
	require.NoError(t, owner.Runtime().DeleteSession(ctx, handle.ID()))
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	frames, ids := legacyStreamFrames(t, string(body))
	require.NotEmpty(t, frames)
	assert.JSONEq(t, `"session_exited"`, string(frames[len(frames)-1]["type"]))
	var stopped bool
	for i, frame := range frames {
		if string(frame["type"]) == `"stream_stopped"` {
			stopped = true
			assert.Positive(t, ids[i])
		}
	}
	assert.True(t, stopped, "ordinary turn terminal is delivered but does not close GET")
}

type legacyFailingWriter struct{ *httptest.ResponseRecorder }

func (*legacyFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLegacyStreamWriteFailureCancelsOnlyAdmittingTurn(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(strconv.FormatBool(owned), func(t *testing.T) {
			handle := &legacyLifecycleHandle{httpSession: &httpSession{id: "session"}}
			writer := &legacyFailingWriter{httptest.NewRecorder()}
			c := echo.New().NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil), writer)
			obs := runtime.Observation{Replay: []runtime.SessionEvent{{SessionID: handle.id, TurnID: "accepted", Event: runtime.StreamStarted(handle.id, "root")}}}
			require.NoError(t, (&Server{}).legacyRunStream(c, handle, obs, "accepted", owned))
			if owned {
				assert.Equal(t, []string{"accepted"}, handle.cancels)
				assert.Equal(t, []string{"accepted"}, handle.awaited)
			} else {
				assert.Empty(t, handle.cancels)
			}
		})
	}
}

func TestLegacyStreamRunPrematureEOFIsActionable(t *testing.T) {
	handle := &httpSession{id: "session"}
	live := make(chan runtime.SessionEvent)
	close(live)
	c, rec := legacyStreamContext(t, t.Context())
	require.NoError(t, (&Server{}).legacyRunStream(c, handle, runtime.Observation{Events: live}, "accepted", true))
	frames, _ := legacyStreamFrames(t, rec.Body.String())
	require.Len(t, frames, 1)
	assert.JSONEq(t, `"observation_ended"`, string(frames[0]["code"]))
	assert.NotContains(t, rec.Body.String(), "session_exited")
	assert.NotContains(t, rec.Body.String(), "stream_stopped")
	assert.Empty(t, handle.cancels)
}

func TestLegacyStreamTerminalWaitsForExactCanonicalSettlement(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			settling := make(chan string, 1)
			settled := make(chan struct{})
			handle := &legacyLifecycleHandle{httpSession: &httpSession{id: "session"}, await: func(ctx context.Context, id string) error {
				settling <- id
				select {
				case <-settled:
					if fail {
						return errors.New("persistence failed")
					}
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			c, rec := legacyStreamContext(t, t.Context())
			obs := runtime.Observation{Replay: []runtime.SessionEvent{{SessionID: handle.id, TurnID: "accepted", Event: runtime.StreamStopped(handle.id, "root", "completed")}}}
			done := make(chan error, 1)
			go func() { done <- (&Server{}).legacyRunStream(c, handle, obs, "accepted", true) }()
			select {
			case id := <-settling:
				assert.Equal(t, "accepted", id)
			case <-time.After(time.Second):
				t.Fatal("terminal did not await settlement")
			}
			select {
			case <-done:
				t.Fatal("handler ended before canonical settlement")
			default:
			}
			assert.Empty(t, rec.Body.String(), "raw terminal must not precede settlement")
			close(settled)
			require.NoError(t, <-done)
			frames, _ := legacyStreamFrames(t, rec.Body.String())
			require.Len(t, frames, 1)
			if fail {
				assert.JSONEq(t, `"settlement_failed"`, string(frames[0]["code"]))
				assert.NotContains(t, rec.Body.String(), "stream_stopped")
			} else {
				assert.JSONEq(t, `"stream_stopped"`, string(frames[0]["type"]))
			}
			assert.Empty(t, handle.cancels)
		})
	}
}

func TestLegacyStreamDisconnectDrainsEvenWhenCancelFails(t *testing.T) {
	handle := &legacyLifecycleHandle{httpSession: &httpSession{id: "session"}, cancelErr: runtime.ErrSessionStopped}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c, _ := legacyStreamContext(t, ctx)
	require.NoError(t, (&Server{}).legacyRunStream(c, handle, runtime.Observation{}, "accepted", true))
	assert.Equal(t, []string{"accepted"}, handle.cancels)
	assert.Equal(t, []string{"accepted"}, handle.awaited)
}
