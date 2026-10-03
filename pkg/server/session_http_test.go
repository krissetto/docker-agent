package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type httpSession struct {
	runtime.UnsupportedSessionHandle

	mu                 sync.Mutex
	id, agent, model   string
	attach             runtime.Observation
	attachOptions      []runtime.ObserveOptions
	submits, steers    []runtime.TurnInput
	responses          []runtime.InteractionResponse
	titles             []string
	cancels            []string
	observationCancels int
	snapshot           *session.Session
	statusErr          error
	steerDisposition   runtime.SubmissionDisposition
	retryDisposition   runtime.SubmissionDisposition
	stopSubtree        bool
	stops              int
	submitErr          error
}

func (a *httpSession) ID() string        { return a.id }
func (a *httpSession) AgentName() string { return a.agent }
func (a *httpSession) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: a.id, AgentName: a.agent, Model: a.model, Capabilities: runtime.SessionCapabilities{StopSubtree: a.stopSubtree}}
}

func (a *httpSession) Submit(_ context.Context, in runtime.TurnInput) (runtime.Submission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.submitErr != nil {
		return runtime.Submission{}, a.submitErr
	}
	a.submits = append(a.submits, in)
	return runtime.Submission{SessionID: a.id, TurnID: "submit-turn"}, nil
}

func (a *httpSession) Retry(context.Context) (runtime.Submission, error) {
	return runtime.Submission{SessionID: a.id, TurnID: "retry-turn", Disposition: a.retryDisposition}, nil
}

func (a *httpSession) Steer(_ context.Context, in runtime.TurnInput) (runtime.Submission, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steers = append(a.steers, in)
	return runtime.Submission{SessionID: a.id, TurnID: "steer-turn", Disposition: a.steerDisposition}, nil
}

func (a *httpSession) Observe(_ context.Context, o runtime.ObserveOptions) (runtime.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attachOptions = append(a.attachOptions, o)
	obs := a.attach
	original := obs.Cancel
	obs.Cancel = func() {
		a.mu.Lock()
		a.observationCancels++
		a.mu.Unlock()
		if original != nil {
			original()
		}
	}
	return obs, nil
}

func (a *httpSession) Snapshot(context.Context) (*session.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.snapshot != nil {
		return a.snapshot.Clone(), nil
	}
	return session.New(session.WithID(a.id), session.WithAgentName(a.agent)), nil
}

func (a *httpSession) Status(context.Context) (runtime.SessionStatus, error) {
	if a.statusErr != nil {
		return runtime.SessionStatus{}, a.statusErr
	}
	return runtime.SessionStatus{SessionID: a.id, AgentName: a.agent, State: runtime.SessionStateRunning}, nil
}

func (a *httpSession) Respond(_ context.Context, r runtime.InteractionResponse) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.responses = append(a.responses, r)
	return nil
}

func (a *httpSession) UpdateTitle(_ context.Context, title string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.titles = append(a.titles, title)
	return nil
}

func (a *httpSession) Cancel(_ context.Context, turn string) (runtime.CancelResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancels = append(a.cancels, turn)
	return runtime.CancelResult{SessionID: a.id, TurnID: turn, Outcome: runtime.CancelAccepted}, nil
}
func (a *httpSession) Release(context.Context) error { return nil }

type httpSessionRegistry struct {
	mu          sync.Mutex
	sessions    map[string]*httpSession
	binding     runtime.SessionBinding
	created     *session.Session
	createCount int
	store       session.Store
	deleted     []string
	submitErr   error
}

func (r *httpSessionRegistry) CreateSession(ctx context.Context, s *session.Session, b runtime.SessionBinding) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.AgentName = b.AgentName
	if r.store != nil {
		if _, err := r.store.GetSession(ctx, s.ID); errors.Is(err, session.ErrNotFound) {
			if err := r.store.AddSession(ctx, s.Clone()); err != nil {
				return nil, err
			}
		}
	}
	a := &httpSession{id: s.ID, agent: b.AgentName, model: b.Model, snapshot: s.Clone(), submitErr: r.submitErr}
	r.sessions[s.ID] = a
	r.binding = b
	r.created = s
	r.createCount++
	return a, nil
}

func (r *httpSessionRegistry) SessionByID(id string) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.sessions[id]; a != nil {
		return a, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}

func (r *httpSessionRegistry) DeleteSession(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, id)
	delete(r.sessions, id)
	if r.store != nil {
		return r.store.DeleteSession(ctx, id)
	}
	return nil
}

type httpTreeRegistry struct {
	*httpSessionRegistry

	muTree    sync.Mutex
	snapshots map[string]*subagent.Snapshot
	inspects  int
	restores  int
}

func (r *httpTreeRegistry) InspectSessionTree(_ context.Context, root string) (*subagent.Snapshot, error) {
	r.muTree.Lock()
	defer r.muTree.Unlock()
	r.inspects++
	return r.snapshots[root], nil
}

func (r *httpTreeRegistry) RestoreSessionTree(context.Context, *session.Session) error {
	r.muTree.Lock()
	defer r.muTree.Unlock()
	r.restores++
	return nil
}

func newSessionHTTPServer(t *testing.T, registry *httpSessionRegistry) (*Server, session.Store) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	registry.store = store
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	return NewWithManager(sm, ""), store
}

func sessionRequest(t *testing.T, s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.e.ServeHTTP(rec, req)
	return rec
}

func TestSessionHTTPCreateUsesStoreAndImmutableBinding(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv, store := newSessionHTTPServer(t, registry)
	rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"agent_name":"root","model":"provider/model","title":"browser"}`, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &metadata))
	assert.NotEmpty(t, metadata.SessionID)
	assert.Equal(t, "root", metadata.AgentName)
	assert.Equal(t, "provider/model", metadata.Model)
	stored, err := store.GetSession(t.Context(), metadata.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "browser", stored.TitleSnapshot())
	assert.Equal(t, runtime.SessionBinding{AgentName: "root", Model: "provider/model"}, registry.binding)
	assert.Equal(t, stored.ID, registry.created.ID)
	assert.Equal(t, "root", stored.AttributesSnapshot()[sessionAgentAttribute])
}

func TestSessionHTTPDelegatesInputsRespondAndCancel(t *testing.T) {
	a := &httpSession{id: "child-session", agent: "worker"}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"child-session": a}}
	srv, _ := newSessionHTTPServer(t, registry)
	multi := []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "part"}}
	for _, mode := range []string{"submit", "steer"} {
		rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/messages", fmt.Sprintf(`{"content":"hello","mode":%q,"request_id":"browser","multi_content":[{"type":"text","text":"part"}]}`, mode), "")
		assert.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	}
	rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/retry", `{}`, "")
	assert.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	rec = sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/child-session/status", "", "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/responses", `{"interaction_id":"request-1","kind":"confirmation","confirmation":"approve"}`, "")
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/cancel", `{"turn_id":"turn-1"}`, "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/child-session/title", `{"title":"renamed"}`, "")
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/messages", `{"content":"hello","mode":"send"}`, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "send is not a mode; submit queues when a turn is running")
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Len(t, a.submits, 1)
	require.Len(t, a.steers, 1)
	assert.Equal(t, multi, a.submits[0].MultiContent)
	require.Len(t, a.responses, 1)
	assert.Equal(t, "request-1", a.responses[0].InteractionID)
	assert.Equal(t, "turn-1", a.cancels[0])
	assert.Equal(t, []string{"renamed"}, a.titles)
}

func TestSessionHTTPTransportsQueuedSteerDisposition(t *testing.T) {
	a := &httpSession{id: "child-session", agent: "worker", steerDisposition: runtime.SubmissionDispositionQueued}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"child-session": a}}
	srv, _ := newSessionHTTPServer(t, registry)
	rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/messages", `{"content":"hello","mode":"steer"}`, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var submission sessionSubmissionDTO
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&submission))
	assert.Equal(t, runtime.SubmissionDispositionQueued, submission.Disposition)
}

func TestSessionHTTPTransportsQueuedRetryDisposition(t *testing.T) {
	a := &httpSession{id: "child-session", agent: "worker", retryDisposition: runtime.SubmissionDispositionQueued}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"child-session": a}}
	srv, _ := newSessionHTTPServer(t, registry)
	rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/child-session/retry", `{}`, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var submission sessionSubmissionDTO
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&submission))
	assert.Equal(t, runtime.SubmissionDispositionQueued, submission.Disposition)
}

func TestSessionHTTPAttachSnapshotReplayLiveAndDisconnectOnlyCancelsObservation(t *testing.T) {
	live := make(chan runtime.SessionEvent, 1)
	cancelled := make(chan struct{})
	a := &httpSession{id: "root", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID("root")), Status: runtime.SessionStatus{SessionID: "root", State: runtime.SessionStateRunning}, Cursor: 2}}, Replay: []runtime.SessionEvent{{SessionID: "root", Sequence: 2, Gap: true, FirstAvailable: 2}}, Events: live, Cancel: func() { close(cancelled) }}}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"root": a}}
	srv, _ := newSessionHTTPServer(t, registry)
	httpSrv := httptest.NewServer(srv.e)
	defer httpSrv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/api/v2/sessions/root/events?since=1", http.NoBody)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	type streamRead struct {
		Type     string              `json:"type"`
		Cursor   uint64              `json:"cursor"`
		Snapshot *sessionSnapshotDTO `json:"snapshot"`
		Envelope *struct {
			Sequence       uint64 `json:"sequence"`
			Gap            bool   `json:"gap"`
			FirstAvailable uint64 `json:"first_available"`
		} `json:"envelope"`
	}
	type streamFrame struct {
		ID, Event string
		Data      streamRead
	}
	readFrame := func() streamFrame {
		var frame streamFrame
		for {
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "id: "):
				frame.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				frame.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame.Data))
				return frame
			}
		}
	}
	firstFrame := readFrame()
	assert.Equal(t, "snapshot", firstFrame.Event)
	assert.Empty(t, firstFrame.ID)
	first := firstFrame.Data
	require.Equal(t, "snapshot", first.Type)
	assert.Equal(t, uint64(2), first.Snapshot.Cursor)
	secondFrame := readFrame()
	assert.Equal(t, "event", secondFrame.Event)
	assert.Equal(t, "2", secondFrame.ID)
	second := secondFrame.Data
	require.NotNil(t, second.Envelope)
	assert.True(t, second.Envelope.Gap)
	assert.Equal(t, uint64(2), second.Envelope.FirstAvailable)
	readyFrame := readFrame()
	assert.Equal(t, "ready", readyFrame.Event)
	assert.Empty(t, readyFrame.ID)
	ready := readyFrame.Data
	require.Equal(t, "ready", ready.Type)
	assert.Equal(t, uint64(2), ready.Cursor)
	live <- runtime.SessionEvent{SessionID: "root", Sequence: 3, Event: runtime.StreamStopped("root", "root", "normal")}
	thirdFrame := readFrame()
	assert.Equal(t, "event", thirdFrame.Event)
	assert.Equal(t, "3", thirdFrame.ID)
	third := thirdFrame.Data
	assert.Equal(t, uint64(3), third.Envelope.Sequence)
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel observation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Len(t, a.attachOptions, 1)
	require.NotNil(t, a.attachOptions[0].Since)
	assert.Equal(t, uint64(1), *a.attachOptions[0].Since)
	assert.Empty(t, a.cancels, "disconnect must not cancel session execution")
}

func TestSessionHTTPCreateUnsupportedWithoutSingleSourceRuntime(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")
	rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"agent_name":"root"}`, "")
	assert.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(runtime.SessionErrorUnsupported))
	sessions, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sessions, "failed session bootstrap must remove its provisional session")
}

// sessionHTTPProvider is an actual LocalRuntime model provider used to exercise
// the public HTTP boundary without a production model dependency.
type sessionHTTPProvider struct{}

func (sessionHTTPProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/http") }
func (sessionHTTPProvider) BaseConfig() base.Config { return base.Config{} }
func (sessionHTTPProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &sessionHTTPStream{}, nil
}

type sessionHTTPStream struct{ index int }

func (s *sessionHTTPStream) Recv() (chat.MessageStreamResponse, error) {
	switch s.index {
	case 0:
		s.index++
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "HTTP integration reply"}}}}, nil
	case 1:
		s.index++
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}
func (*sessionHTTPStream) Close() {}

func TestSessionHTTPLocalRuntimeCreateAttachSubmitSettlement(t *testing.T) {
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	handler := NewWithManager(sm, "")
	handler.heartbeatInterval = 10 * time.Millisecond
	var eventRequests atomic.Int32
	handler.e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodGet && strings.HasSuffix(c.Request().URL.Path, "/events") {
				eventRequests.Add(1)
			}
			return next(c)
		}
	})
	srv := httptest.NewServer(handler.e)
	defer srv.Close()

	createReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v2/sessions", strings.NewReader(`{"agent_name":"root"}`))
	require.NoError(t, err)
	createReq.Header.Set("Content-Type", "application/json")
	create, err := http.DefaultClient.Do(createReq)
	require.NoError(t, err)
	defer create.Body.Close()
	require.Equal(t, http.StatusCreated, create.StatusCode)
	var metadata sessionMetadataDTO
	require.NoError(t, json.NewDecoder(create.Body).Decode(&metadata))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attachReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v2/sessions/"+metadata.SessionID+"/events", http.NoBody)
	require.NoError(t, err)
	attached, err := http.DefaultClient.Do(attachReq)
	require.NoError(t, err)
	defer attached.Body.Close()
	reader := bufio.NewReader(attached.Body)
	readMessageE := func() (map[string]any, error) {
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return nil, readErr
			}
			if strings.HasPrefix(line, "data: ") {
				var message map[string]any
				decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "data: "))))
				decoder.UseNumber()
				if err := decoder.Decode(&message); err != nil {
					return nil, err
				}
				return message, nil
			}
		}
	}
	readMessage := func() map[string]any {
		message, readErr := readMessageE()
		require.NoError(t, readErr)
		return message
	}
	sequenceOf := func(envelope map[string]any) uint64 {
		number, ok := envelope["sequence"].(json.Number)
		require.True(t, ok, "canonical sequence must be an integer")
		sequence, err := strconv.ParseUint(number.String(), 10, 64)
		require.NoError(t, err)
		return sequence
	}
	assert.Equal(t, "snapshot", readMessage()["type"])
	assert.Equal(t, "ready", readMessage()["type"])

	submitReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v2/sessions/"+metadata.SessionID+"/messages", strings.NewReader(`{"content":"hello"}`))
	require.NoError(t, err)
	submitReq.Header.Set("Content-Type", "application/json")
	submit, err := http.DefaultClient.Do(submitReq)
	require.NoError(t, err)
	defer submit.Body.Close()
	require.Equal(t, http.StatusAccepted, submit.StatusCode)
	var firstSubmission sessionSubmissionDTO
	require.NoError(t, json.NewDecoder(submit.Body).Decode(&firstSubmission))

	var lastSequence uint64
	for {
		message := readMessage()
		envelope, _ := message["envelope"].(map[string]any)
		sequence := sequenceOf(envelope)
		assert.Greater(t, sequence, lastSequence)
		lastSequence = sequence
		assert.Equal(t, metadata.SessionID, envelope["session_id"])
		assert.Equal(t, firstSubmission.TurnID, envelope["turn_id"])
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "stream_stopped" {
			break
		}
	}
	// StreamStopped remains delivered first. Durable settlement is the exact
	// next canonical event, not an idle observation or a new execution turn.
	settlement := readMessage()
	settledEnvelope, ok := settlement["envelope"].(map[string]any)
	require.True(t, ok)
	settledSequence := sequenceOf(settledEnvelope)
	assert.Equal(t, lastSequence+1, settledSequence)
	lastSequence = settledSequence
	assert.Equal(t, metadata.SessionID, settledEnvelope["session_id"])
	assert.Equal(t, firstSubmission.TurnID, settledEnvelope["turn_id"])
	settledEvent, ok := settledEnvelope["event"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "turn_settled", settledEvent["type"])
	assert.Equal(t, metadata.SessionID, settledEvent["session_id"])
	assert.Equal(t, firstSubmission.TurnID, settledEvent["turn_id"])
	assert.Equal(t, string(runtime.TurnCompleted), settledEvent["outcome"])
	require.Eventually(t, func() bool {
		handle, lookupErr := owner.Runtime().SessionByID(metadata.SessionID)
		if lookupErr != nil {
			return false
		}
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.State == runtime.SessionStateSettled && status.Pending == 0
	}, 3*time.Second, 10*time.Millisecond)

	// Settlement is an event on the existing session observation, not the end
	// of that observation. Block on the original response for long enough to
	// prove it remains open while idle, then require a second turn on that same
	// reader without another events request.
	type messageResult struct {
		message map[string]any
		err     error
	}
	next := make(chan messageResult, 1)
	go func() {
		message, readErr := readMessageE()
		next <- messageResult{message: message, err: readErr}
	}()
	select {
	case result := <-next:
		if result.err != nil {
			t.Fatalf("original event response closed after first settlement: %v", result.err)
		}
		t.Fatalf("unexpected event while session was idle: %#v", result.message)
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, int32(1), eventRequests.Load())

	secondReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v2/sessions/"+metadata.SessionID+"/messages", strings.NewReader(`{"content":"again"}`))
	require.NoError(t, err)
	secondReq.Header.Set("Content-Type", "application/json")
	second, err := http.DefaultClient.Do(secondReq)
	require.NoError(t, err)
	defer second.Body.Close()
	require.Equal(t, http.StatusAccepted, second.StatusCode)
	var secondSubmission sessionSubmissionDTO
	require.NoError(t, json.NewDecoder(second.Body).Decode(&secondSubmission))

	var secondTypes []string
	for {
		var message map[string]any
		if len(secondTypes) == 0 {
			result := <-next
			require.NoError(t, result.err)
			message = result.message
		} else {
			message = readMessage()
		}
		envelope, _ := message["envelope"].(map[string]any)
		sequence := sequenceOf(envelope)
		assert.Greater(t, sequence, lastSequence)
		lastSequence = sequence
		assert.Equal(t, secondSubmission.TurnID, envelope["turn_id"])
		event, _ := envelope["event"].(map[string]any)
		eventType, _ := event["type"].(string)
		secondTypes = append(secondTypes, eventType)
		if eventType == "stream_stopped" {
			break
		}
	}
	wantSecond := []string{"pending_user_message_accepted", "pending_user_message_promoted", "stream_started", "agent_choice", "message_added", "stream_stopped"}
	wantIndex := 0
	for _, eventType := range secondTypes {
		if wantIndex < len(wantSecond) && eventType == wantSecond[wantIndex] {
			wantIndex++
		}
	}
	assert.Equal(t, len(wantSecond), wantIndex, "second-turn events out of order: %v", secondTypes)
	assert.Equal(t, int32(1), eventRequests.Load())
}

type sessionHTTPCancelProvider struct{ started chan struct{} }

func (sessionHTTPCancelProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/http-cancel")
}
func (sessionHTTPCancelProvider) BaseConfig() base.Config { return base.Config{} }
func (p sessionHTTPCancelProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return &sessionHTTPCancelStream{ctx: ctx, started: p.started}, nil
}

type sessionHTTPCancelStream struct {
	ctx     context.Context //nolint:containedctx // stream cancellation is driven by the provider request context
	started chan struct{}
	once    sync.Once
}

func (s *sessionHTTPCancelStream) Recv() (chat.MessageStreamResponse, error) {
	s.once.Do(func() { close(s.started) })
	<-s.ctx.Done()
	return chat.MessageStreamResponse{}, s.ctx.Err()
}
func (*sessionHTTPCancelStream) Close() {}

func TestSessionHTTPLocalRuntimeCorrelatedCancel(t *testing.T) {
	started := make(chan struct{})
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(sessionHTTPCancelProvider{started: started})))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	srv := httptest.NewServer(NewWithManager(sm, "").e)
	defer srv.Close()

	postJSON := func(path, body string, out any) int {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
		require.NoError(t, requestErr)
		request.Header.Set("Content-Type", "application/json")
		response, postErr := http.DefaultClient.Do(request)
		require.NoError(t, postErr)
		defer response.Body.Close()
		if out != nil {
			require.NoError(t, json.NewDecoder(response.Body).Decode(out))
		}
		return response.StatusCode
	}
	var metadata sessionMetadataDTO
	require.Equal(t, http.StatusCreated, postJSON("/api/v2/sessions", `{"agent_name":"root"}`, &metadata))
	var submission sessionSubmissionDTO
	require.Equal(t, http.StatusAccepted, postJSON("/api/v2/sessions/"+metadata.SessionID+"/messages", `{"content":"block"}`, &submission))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}

	var canceled struct {
		TurnID  string                `json:"turn_id"`
		Outcome runtime.CancelOutcome `json:"outcome"`
	}
	require.Equal(t, http.StatusOK, postJSON("/api/v2/sessions/"+metadata.SessionID+"/cancel", `{"turn_id":"`+submission.TurnID+`"}`, &canceled))
	assert.Equal(t, submission.TurnID, canceled.TurnID)
	assert.Equal(t, runtime.CancelAccepted, canceled.Outcome)
	require.Eventually(t, func() bool {
		handle, lookupErr := owner.Runtime().SessionByID(metadata.SessionID)
		if lookupErr != nil {
			return false
		}
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.State == runtime.SessionStateSettled
	}, 3*time.Second, 10*time.Millisecond)

	var inactive struct {
		Outcome runtime.CancelOutcome `json:"outcome"`
	}
	require.Equal(t, http.StatusOK, postJSON("/api/v2/sessions/"+metadata.SessionID+"/cancel", `{"turn_id":"`+submission.TurnID+`"}`, &inactive))
	assert.Equal(t, runtime.CancelNotActive, inactive.Outcome)
}

func TestSessionHTTPRejectsWorkingDirAndRoutesExplicitSource(t *testing.T) {
	first := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	second := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	store := session.NewInMemorySessionStore()
	first.store, second.store = store, store
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimes(map[string]runtime.SessionRuntime{"first.yaml": first, "second.yaml": second}))
	srv := NewWithManager(sm, "")

	ambiguous := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"agent_name":"root"}`, "")
	assert.Equal(t, http.StatusBadRequest, ambiguous.Code, ambiguous.Body.String())
	for _, workingDir := range []string{"/tmp/client", "../escape", "sibling", "/tmp/symlink-outside"} {
		explicitWorkingDir := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", fmt.Sprintf(`{"source":"first.yaml","agent_name":"root","working_dir":%q}`, workingDir), "")
		assert.Equal(t, http.StatusBadRequest, explicitWorkingDir.Code, explicitWorkingDir.Body.String())
	}

	created := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"source":"second.yaml","agent_name":"root"}`, "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))
	assert.Nil(t, first.created)
	require.NotNil(t, second.created)
	assert.Empty(t, second.created.WorkingDir)
	stored, err := store.GetSession(t.Context(), metadata.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "second.yaml", stored.AttributesSnapshot()[sessionSourceAttribute])

	status := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+metadata.SessionID+"/status", "", "")
	assert.Equal(t, http.StatusOK, status.Code, status.Body.String())
}

func TestSessionHTTPCatalogListsForestsSourcesAndPersistedSessions(t *testing.T) {
	first := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	secondSession := &httpSession{id: "live-child", agent: "worker", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Status: runtime.SessionStatus{SessionID: "live-child", AgentName: "worker", State: runtime.SessionStateRunning, Pending: 2}, PendingInputs: []runtime.PendingInput{{TurnID: "p1"}, {TurnID: "p2"}}, Interactions: []runtime.InteractionSnapshot{{InteractionID: "i1"}}}}, Cancel: func() {}}}
	second := &httpSessionRegistry{sessions: map[string]*httpSession{"live-child": secondSession}}
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("stored-root"), session.WithAgentName("root"), session.WithTitle("Stored root"), session.WithAttributes(map[string]string{sessionSourceAttribute: "first.yaml", sessionAgentAttribute: "root"}))
	child := session.New(session.WithID("live-child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithTitle("Live child"), session.WithAttributes(map[string]string{sessionSourceAttribute: "second.yaml", sessionAgentAttribute: "worker"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	secondSession.snapshot = child.Clone()
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimes(map[string]runtime.SessionRuntime{"second.yaml": second, "first.yaml": first}))
	sm.runtimeSessions.Store(child.ID, &activeRuntimes{handle: secondSession, registry: second})
	srv := NewWithManager(sm, "secret")

	unauthorized := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions?include_children=true", "", "")
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions?include_children=true", "", "secret")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var catalog sessionCatalogDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
	assert.Equal(t, 2, catalog.Version)
	assert.Equal(t, []sessionCatalogSourceDTO{{Name: "first.yaml", CanCreate: true}, {Name: "second.yaml", CanCreate: true}}, catalog.Sources)
	require.Len(t, catalog.Sessions, 2)
	byID := map[string]sessionResourceDTO{}
	for _, session := range catalog.Sessions {
		byID[session.SessionID] = session
	}
	assert.Empty(t, byID[root.ID].ParentID)
	assert.Equal(t, "first.yaml", byID[root.ID].Source)
	assert.True(t, byID[root.ID].Loadable)
	assert.True(t, byID[root.ID].Attachable)
	assert.False(t, byID[root.ID].Loaded)
	assert.Equal(t, root.ID, byID[child.ID].ParentID)
	assert.Equal(t, runtime.SessionStateRunning, byID[child.ID].State)
	require.NotNil(t, byID[child.ID].Pending)
	assert.Equal(t, 0, *byID[child.ID].Pending)
	assert.Nil(t, byID[child.ID].Interactions, "catalog does not attach merely to count interactions")
	assert.True(t, byID[child.ID].Loaded)
}

func TestSessionHTTPPersistedSessionRestoresUsingServerOwnedSource(t *testing.T) {
	first := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	second := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	store := session.NewInMemorySessionStore()
	first.store, second.store = store, store
	persisted := session.New(session.WithID("persisted"), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionSourceAttribute: "second.yaml", sessionAgentAttribute: "worker"}))
	require.NoError(t, store.AddSession(t.Context(), persisted))
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimes(map[string]runtime.SessionRuntime{"first.yaml": first, "second.yaml": second}))
	srv := NewWithManager(sm, "")

	rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/persisted/status", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, first.created)
	require.NotNil(t, second.created)
	assert.Equal(t, "persisted", second.created.ID)
	assert.Equal(t, "worker", second.binding.AgentName)

	ambiguous := session.New(session.WithID("ambiguous"), session.WithAgentName("worker"))
	require.NoError(t, store.AddSession(t.Context(), ambiguous))
	failed := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/ambiguous/status", "", "")
	assert.Equal(t, http.StatusNotImplemented, failed.Code, failed.Body.String())
	assert.Contains(t, failed.Body.String(), `"error":"unsupported"`)
	assert.Contains(t, failed.Body.String(), `"operation":"attach"`)
	assert.Equal(t, "persisted", second.created.ID, "caller ID must not route an ambiguous source")
}

func TestSessionHTTPLocalRuntimeColdChildRestoresRootTree(t *testing.T) {
	for _, factoryRouting := range []bool{false, true} {
		t.Run(fmt.Sprintf("factory=%t", factoryRouting), func(t *testing.T) {
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			provider := sessionHTTPProvider{}
			workspace := t.TempDir()
			newRuntime := func() runtime.SessionRuntimeSupervisor {
				rt, runtimeErr := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
					agent.New("root", "prompt", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
					agent.New("worker", "prompt", agent.WithModel(provider)),
				)), runtime.WithSessionStore(store), runtime.WithWorkingDir(normalizeDir(workspace)))
				require.NoError(t, runtimeErr)
				return runtime.NewSessionRuntimeSupervisor(rt)
			}
			root := session.New(session.WithID("persisted-root"), session.WithWorkingDir(workspace), session.WithAgentName("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
			child := session.New(session.WithID("persisted-child"), session.WithWorkingDir(workspace), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
			require.NoError(t, store.AddSession(t.Context(), root))
			require.NoError(t, store.AddSession(t.Context(), child))
			treeRoot := subagent.SessionRootID(root.ID)
			require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), root.ID, subagent.Snapshot{Root: treeRoot, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: treeRoot, Agent: "root", State: subagent.NodeRunning}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "worker-node", Agent: "worker", Parent: treeRoot, SessionID: child.ID, State: subagent.NodeIdle}}}}}}))

			var sm *SessionManager
			if factoryRouting {
				factory := &factoryRecorder{store: store}
				_, sm = newFactoryServer(t, factory, &memorySource{data: "agents: {}"})
			} else {
				owner := newRuntime()
				t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
				sm = NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
			}
			srv := NewWithManager(sm, "")
			const callers = 12
			start := make(chan struct{})
			codes := make(chan int, callers)
			var wg sync.WaitGroup
			for range callers {
				wg.Go(func() {
					<-start
					codes <- sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+child.ID+"/status", "", "").Code
				})
			}
			close(start)
			wg.Wait()
			close(codes)
			for code := range codes {
				assert.Equal(t, http.StatusOK, code)
			}

			submit := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/"+child.ID+"/messages", `{"content":"continue"}`, "")
			require.Equal(t, http.StatusAccepted, submit.Code, submit.Body.String())
			httpServer := httptest.NewServer(srv.e)
			defer httpServer.Close()
			attachCtx, cancelAttach := context.WithCancel(t.Context())
			attachReq, err := http.NewRequestWithContext(attachCtx, http.MethodGet, httpServer.URL+"/api/v2/sessions/"+child.ID+"/events", http.NoBody)
			require.NoError(t, err)
			attached, err := http.DefaultClient.Do(attachReq)
			require.NoError(t, err)
			reader := bufio.NewReader(attached.Body)
			for {
				line, readErr := reader.ReadString('\n')
				require.NoError(t, readErr)
				if strings.HasPrefix(line, "data: ") {
					assert.Contains(t, line, `"type":"snapshot"`)
					break
				}
			}
			cancelAttach()
			require.NoError(t, attached.Body.Close())

			_, rootPublished := sm.runtimeSessions.Load(root.ID)
			childActive, childPublished := sm.runtimeSessions.Load(child.ID)
			assert.True(t, rootPublished)
			require.True(t, childPublished)
			assert.Equal(t, child.ID, childActive.handle.ID())
			assert.Equal(t, "worker", childActive.handle.AgentName())
		})
	}
}

func TestSessionHTTPCatalogDoesNotCreateObserversAndKeepsUnknownStatus(t *testing.T) {
	a := &httpSession{id: "session", agent: "root", statusErr: errors.New("status unavailable")}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"session": a}}
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("session"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{handle: a, registry: registry})
	srv := NewWithManager(sm, "")
	for range 3 {
		rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions", "", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var catalog sessionCatalogDTO
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
		require.Len(t, catalog.Sessions, 1)
		assert.False(t, catalog.Sessions[0].StateKnown)
		assert.Empty(t, catalog.Sessions[0].State)
		assert.Nil(t, catalog.Sessions[0].Pending)
		assert.Nil(t, catalog.Sessions[0].Interactions)
		assert.Equal(t, "session status unavailable", catalog.Sessions[0].RouteError)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	assert.Empty(t, a.attachOptions)
	assert.Zero(t, a.observationCancels)
}

func TestSessionHTTPConcurrentColdRootRestorePublishesOnce(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("cold-root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "")

	const callers = 24
	start := make(chan struct{})
	codes := make(chan int, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			codes <- sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/cold-root/status", "", "").Code
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	for code := range codes {
		assert.Equal(t, http.StatusOK, code)
	}
	registry.mu.Lock()
	assert.Equal(t, 1, registry.createCount)
	winner := registry.sessions[root.ID]
	registry.mu.Unlock()
	active, ok := sm.runtimeSessions.Load(root.ID)
	require.True(t, ok)
	assert.Same(t, winner, active.handle)
	assert.Zero(t, sm.sessionRestoreLocks.length(), "completed concurrent waiters release keyed lock")
}

// These catalog fixtures include historical child rows, unlike the default
// root-only store catalog, so membership authorization is exercised directly.
type childCatalogStore struct {
	session.Store

	ids []string
}

func (s childCatalogStore) GetSessionSummaryPage(ctx context.Context, options session.SummaryPageOptions) (session.SummaryPage, error) {
	return s.Store.(session.PagedSummaryStore).GetSessionSummaryPage(ctx, options)
}

func (s childCatalogStore) GetSessions(ctx context.Context) ([]*session.Session, error) {
	rows := make([]*session.Session, 0, len(s.ids))
	for _, id := range s.ids {
		row, err := s.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func TestSessionCatalogDefersDurableMembershipUntilSelection(t *testing.T) {
	root := session.New(session.WithID("root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
	nested := session.New(session.WithID("nested"), session.WithParentID(child.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
	orphan := session.New(session.WithID("orphan"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
	store := childCatalogStore{Store: session.NewInMemorySessionStore(), ids: []string{root.ID, child.ID, nested.ID, orphan.ID}}
	for _, sess := range []*session.Session{root, child, nested, orphan} {
		require.NoError(t, store.AddSession(t.Context(), sess))
	}
	rootNode := subagent.SessionRootID(root.ID)
	snapshot := &subagent.Snapshot{Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Agent: "worker", Parent: rootNode, SessionID: child.ID}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "nested-node", Agent: "worker", Parent: "child-node", SessionID: nested.ID}}}}}}}}
	registry := &httpTreeRegistry{httpSessionRegistry: &httpSessionRegistry{sessions: map[string]*httpSession{}}, snapshots: map[string]*subagent.Snapshot{root.ID: snapshot}}
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "")
	rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions?include_children=true", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var catalog sessionCatalogDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
	byID := map[string]sessionResourceDTO{}
	for _, entry := range catalog.Sessions {
		byID[entry.SessionID] = entry
	}
	require.Contains(t, byID, child.ID)
	require.Contains(t, byID, nested.ID)
	require.Contains(t, byID, orphan.ID)
	assert.False(t, byID[child.ID].Attachable)
	assert.True(t, byID[child.ID].RequiresConfirmation)
	assert.False(t, byID[nested.ID].Attachable)
	assert.True(t, byID[nested.ID].RequiresConfirmation)
	assert.False(t, byID[orphan.ID].Attachable)
	assert.True(t, byID[orphan.ID].RequiresConfirmation, "catalog rows are candidates; only confirmed selection validates tree membership")
	registry.muTree.Lock()
	defer registry.muTree.Unlock()
	assert.Zero(t, registry.inspects, "catalog must not inspect durable trees")
}

func TestSessionCatalogRejectsTreeBindingMismatches(t *testing.T) {
	for _, tc := range []struct {
		name, rowParent, rowAgent, treeParent, treeAgent, want string
	}{
		{"parent", "other", "worker", "root", "worker", "durable tree parent session does not match persisted parent_id"},
		{"agent", "root", "worker", "root", "other", "durable session tree agent does not match persisted agent configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := session.New(session.WithID("root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
			other := session.New(session.WithID("other"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
			child := session.New(session.WithID("child"), session.WithParentID(tc.rowParent), session.WithAgentName(tc.rowAgent), session.WithAttributes(map[string]string{sessionAgentAttribute: tc.rowAgent}))
			store := childCatalogStore{Store: session.NewInMemorySessionStore(), ids: []string{root.ID, other.ID, child.ID}}
			for _, sess := range []*session.Session{root, other, child} {
				require.NoError(t, store.AddSession(t.Context(), sess))
			}
			rootNode := subagent.SessionRootID(root.ID)
			treeParentNode, parentSession := rootNode, root.ID
			children := []subagent.NodeSnapshot{}
			if tc.treeParent == "other" {
				treeParentNode, parentSession = "other-node", other.ID
				children = append(children, subagent.NodeSnapshot{Node: subagent.Node{ID: treeParentNode, Agent: "worker", Parent: rootNode, SessionID: other.ID}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Agent: tc.treeAgent, Parent: treeParentNode, SessionID: child.ID}}}})
			} else {
				children = append(children, subagent.NodeSnapshot{Node: subagent.Node{ID: "child-node", Agent: tc.treeAgent, Parent: treeParentNode, SessionID: child.ID}})
			}
			_ = parentSession
			snapshot := &subagent.Snapshot{Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: children}}}
			registry := &httpTreeRegistry{httpSessionRegistry: &httpSessionRegistry{sessions: map[string]*httpSession{}}, snapshots: map[string]*subagent.Snapshot{root.ID: snapshot}}
			sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
			rec := sessionRequest(t, NewWithManager(sm, ""), http.MethodGet, "/api/v2/sessions?include_children=true", "", "")
			var catalog sessionCatalogDTO
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
			foundChild := false
			for _, entry := range catalog.Sessions {
				if entry.SessionID == child.ID {
					foundChild = true
					assert.False(t, entry.Attachable)
					assert.True(t, entry.RequiresConfirmation, "metadata is not an access grant")
				}
			}
			require.True(t, foundChild, "catalog must include child authorization fixture")
			direct := sessionRequest(t, NewWithManager(sm, ""), http.MethodGet, "/api/v2/sessions/child/status", "", "")
			assert.Equal(t, http.StatusBadRequest, direct.Code, "confirmed selection must reject the mismatch")
		})
	}
}

func TestSessionTreeRootIdentityAndBindingRejectCatalogAndColdRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, treeRoot, treeAgent, want string
	}{
		{"wrong-root-id", "synthetic-root", "root", "durable session tree root id does not match persisted root session"},
		{"root-agent", "", "other", "durable session tree root agent does not match persisted root configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := session.New(session.WithID("root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
			child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
			store := childCatalogStore{Store: session.NewInMemorySessionStore(), ids: []string{root.ID, child.ID}}
			require.NoError(t, store.AddSession(t.Context(), root))
			require.NoError(t, store.AddSession(t.Context(), child))
			treeRoot := subagent.NodeID(tc.treeRoot)
			if treeRoot == "" {
				treeRoot = subagent.SessionRootID(root.ID)
			}
			snapshot := &subagent.Snapshot{
				Root: treeRoot,
				Nodes: []subagent.NodeSnapshot{{
					Node:     subagent.Node{ID: treeRoot, Agent: tc.treeAgent},
					Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Agent: "worker", Parent: treeRoot, SessionID: child.ID}}},
				}},
			}
			registry := &httpTreeRegistry{httpSessionRegistry: &httpSessionRegistry{sessions: map[string]*httpSession{}}, snapshots: map[string]*subagent.Snapshot{root.ID: snapshot}}
			sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
			srv := NewWithManager(sm, "")

			catalogRec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions?include_children=true", "", "")
			var catalog sessionCatalogDTO
			require.NoError(t, json.Unmarshal(catalogRec.Body.Bytes(), &catalog))
			foundChild := false
			for _, entry := range catalog.Sessions {
				if entry.SessionID == child.ID {
					foundChild = true
					assert.False(t, entry.Attachable)
					assert.True(t, entry.RequiresConfirmation, "metadata is not an access grant")
				}
			}
			require.True(t, foundChild, "catalog must include child authorization fixture")
			direct := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/child/status", "", "")
			assert.Equal(t, http.StatusBadRequest, direct.Code, direct.Body.String())
			registry.mu.Lock()
			assert.Zero(t, registry.createCount)
			registry.mu.Unlock()
			registry.muTree.Lock()
			assert.Zero(t, registry.restores)
			registry.muTree.Unlock()
			assert.Zero(t, sm.runtimeSessions.Length())
		})
	}
}

func TestValidateSessionTreeRejectsMalformedAndDuplicateMembership(t *testing.T) {
	rootSession := session.New(session.WithID("root"), session.WithAgentName("root"))
	root := subagent.SessionRootID(rootSession.ID)
	for _, tc := range []struct {
		name     string
		snapshot *subagent.Snapshot
		want     string
	}{
		{"malformed-root", &subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "wrong", Agent: "root"}}}}, "durable session tree root is malformed"},
		{"duplicate-node", &subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "same", Agent: "worker", Parent: root, SessionID: "a"}}, {Node: subagent.Node{ID: "same", Agent: "worker", Parent: root, SessionID: "b"}}}}}}, "durable session tree contains duplicate node id"},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.EqualError(t, validateSessionTree(rootSession, tc.snapshot).err, tc.want) })
	}
	duplicateSession := &subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "a", Agent: "worker", Parent: root, SessionID: "child"}}, {Node: subagent.Node{ID: "b", Agent: "worker", Parent: root, SessionID: "child"}}}}}}
	validated := validateSessionTree(rootSession, duplicateSession)
	require.NoError(t, validated.err)
	assert.Equal(t, 2, validated.members["child"].count)
}

func TestSessionRestoreLocksCleanupSuccessAndFailure(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	store := session.NewInMemorySessionStore()
	good := session.New(session.WithID("good"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	bad := session.New(session.WithID("bad"))
	require.NoError(t, store.AddSession(t.Context(), good))
	require.NoError(t, store.AddSession(t.Context(), bad))
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "")
	assert.Equal(t, http.StatusOK, sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/good/status", "", "").Code)
	assert.Equal(t, http.StatusNotImplemented, sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/bad/status", "", "").Code)
	assert.Zero(t, sm.sessionRestoreLocks.length())
}

func TestSessionHTTPMultiSourceDeleteLiveAndReloaded(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "live", false: "reloaded"}[live], func(t *testing.T) {
			first := &httpSessionRegistry{sessions: map[string]*httpSession{}}
			second := &httpSessionRegistry{sessions: map[string]*httpSession{}}
			store := session.NewInMemorySessionStore()
			first.store, second.store = store, store
			newServer := func() *Server {
				sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimes(map[string]runtime.SessionRuntime{"first.yaml": first, "second.yaml": second}))
				return NewWithManager(sm, "")
			}
			srv := newServer()
			created := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"source":"second.yaml","agent_name":"root"}`, "")
			require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			var metadata sessionMetadataDTO
			require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))
			if !live {
				srv = newServer()
			}
			deleted := sessionRequest(t, srv, http.MethodDelete, "/api/v2/sessions/"+metadata.SessionID, "", "")
			require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
			_, err := store.GetSession(t.Context(), metadata.SessionID)
			require.Error(t, err)
			assert.Empty(t, first.deleted)
			assert.Equal(t, []string{metadata.SessionID}, second.deleted)
		})
	}
}

func TestLegacyExecutionRoutesAbsentSessionStillExecutes(t *testing.T) {
	a := &httpSession{id: "session", agent: "root"}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"session": a}}
	srv, _ := newSessionHTTPServer(t, registry)
	for _, path := range []string{"/api/v2/sessions/session/agent/team.yaml", "/api/v2/sessions/session/agent/team.yaml/root"} {
		rec := sessionRequest(t, srv, http.MethodPost, path, `{}`, "")
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	}
	rec := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/session/messages", `{"content":"hello"}`, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Len(t, a.submits, 1)
}

func TestSessionHTTPStrictBodies(t *testing.T) {
	a := &httpSession{id: "session", agent: "root"}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"session": a}}
	srv, _ := newSessionHTTPServer(t, registry)
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v2/sessions/session/messages", `{"content":"x","unknown":true}`},
		{http.MethodPost, "/api/v2/sessions/session/messages", `{"content":"x","input_origin":"runtime"}`},
		{http.MethodPost, "/api/v2/sessions/session/messages", `{"content":"x","sender_id":"child","sender_name":"worker"}`},
		{http.MethodPost, "/api/v2/sessions/session/messages", `{"content":"x"} {}`},
		{http.MethodPatch, "/api/v2/sessions/session/title", `{"title":"x","unknown":true}`},
		{http.MethodPost, "/api/v2/sessions/session/responses", `{"interaction_id":"i","kind":"confirmation","confirmation":"approve","unknown":true}`},
		{http.MethodPost, "/api/v2/sessions/session/cancel", `{"turn_id":"t","unknown":true}`},
		{http.MethodPost, "/api/v2/sessions/session/retry", `{"unknown":true}`},
	}
	for _, tc := range cases {
		rec := sessionRequest(t, srv, tc.method, tc.path, tc.body, "")
		assert.Equal(t, http.StatusBadRequest, rec.Code, tc.path+": "+rec.Body.String())
	}
}

func TestSessionHTTPBodyLimitsAndAttachUnaffected(t *testing.T) {
	live := make(chan runtime.SessionEvent)
	a := &httpSession{id: "session", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID("session")), Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateSettled}}}, Events: live, Cancel: func() {}}}
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{"session": a}}
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "", WithMaxRequestBytes(128))
	oversized := strings.Repeat("x", 256)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v2/sessions", fmt.Sprintf(`{"agent_name":"root","title":%q}`, oversized)},
		{http.MethodPost, "/api/v2/sessions/session/messages", fmt.Sprintf(`{"content":%q}`, oversized)},
		{http.MethodPost, "/api/v2/sessions/session/responses", fmt.Sprintf(`{"interaction_id":"i","kind":"elicitation","action":"accept","content":{"x":%q}}`, oversized)},
	} {
		rec := sessionRequest(t, srv, tc.method, tc.path, tc.body, "")
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, tc.path+": "+rec.Body.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v2/sessions/session/events", nil)
	rec := httptest.NewRecorder()
	srv.e.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestCanonicalSessionRouteInventoryAndSessionNamespaceAbsent(t *testing.T) {
	srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{}})
	want := map[string]bool{
		"GET /api/v2/sessions": true, "POST /api/v2/sessions": true, "POST /api/v2/sessions/start": true,
		"GET /api/v2/sessions/:id": true, "GET /api/v2/sessions/:id/status": true,
		"GET /api/v2/sessions/:id/snapshot": true, "GET /api/v2/sessions/:id/events": true,
		"POST /api/v2/sessions/:id/messages": true, "POST /api/v2/sessions/:id/retry": true,
		"POST /api/v2/sessions/:id/responses": true, "POST /api/v2/sessions/:id/cancel": true, "POST /api/v2/sessions/:id/stop-subtree": true,
		"PATCH /api/v2/sessions/:id/title": true, "GET /api/v2/sessions/:id/tree": true,
		"DELETE /api/v2/sessions/:id": true,
	}
	seen := map[string]int{}
	for _, route := range srv.e.Routes() {
		key := route.Method + " " + route.Path
		seen[key]++
		if strings.Contains(route.Path, "/act"+"ors") {
			t.Fatalf("retired route registered: %s", key)
		}
		delete(want, key)
	}
	assert.Empty(t, want)
	for route, count := range seen {
		if strings.HasPrefix(route, "GET /api/v2/sessions") || strings.HasPrefix(route, "POST /api/v2/sessions") || strings.HasPrefix(route, "PATCH /api/v2/sessions") || strings.HasPrefix(route, "DELETE /api/v2/sessions") {
			assert.Equal(t, 1, count, route)
		}
	}
	for _, old := range []string{"POST /api/v2/sessions/:id/resume", "POST /api/v2/sessions/:id/elicitation", "POST /api/v2/sessions/:id/steer", "POST /api/v2/sessions/:id/followup"} {
		assert.Zero(t, seen[old], old)
	}
	assert.Equal(t, http.StatusNotAcceptable, sessionRequest(t, srv, http.MethodGet, "/api/v99/sessions", "", "").Code)
}

func TestLegacySessionInspectableButNotAttachable(t *testing.T) {
	store := session.NewInMemorySessionStore()
	legacy := session.New(session.WithID("legacy"), session.WithTitle("old row"))
	require.NoError(t, store.AddSession(t.Context(), legacy))
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(&httpSessionRegistry{sessions: map[string]*httpSession{}}))
	srv := NewWithManager(sm, "")
	assert.Equal(t, http.StatusOK, sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/legacy", "", "").Code)
	rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/legacy/events", "", "")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Contains(t, rec.Body.String(), `"error":"unsupported"`)
	assert.Contains(t, rec.Body.String(), `"operation":"attach"`)
}

func TestSessionEventsLastEventIDPrecedenceAndParseErrors(t *testing.T) {
	newServer := func() (*Server, *httpSession) {
		a := &httpSession{id: "s", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID("s")), Status: runtime.SessionStatus{SessionID: "s"}}}, Events: make(chan runtime.SessionEvent), Cancel: func() {}}}
		return NewWithManager(NewSessionManager(t.Context(), config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}, WithSessionRuntime(&httpSessionRegistry{sessions: map[string]*httpSession{"s": a}})), ""), a
	}

	for _, tc := range []struct {
		name, query, header string
		want                uint64
	}{
		{name: "header fallback", header: "7", want: 7},
		{name: "query wins", query: "9", header: "7", want: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, handle := newServer()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v2/sessions/s/events?since="+tc.query, nil)
			req.Header.Set("Last-Event-ID", tc.header)
			srv.e.ServeHTTP(httptest.NewRecorder(), req)
			handle.mu.Lock()
			defer handle.mu.Unlock()
			require.Len(t, handle.attachOptions, 1)
			require.NotNil(t, handle.attachOptions[0].Since)
			assert.Equal(t, tc.want, *handle.attachOptions[0].Since)
		})
	}

	for _, tc := range []struct{ query, header string }{{query: "bad"}, {header: "bad"}} {
		srv, handle := newServer()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v2/sessions/s/events?since="+tc.query, nil)
		req.Header.Set("Last-Event-ID", tc.header)
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), `"error":"invalid_request"`)
		handle.mu.Lock()
		assert.Empty(t, handle.attachOptions)
		handle.mu.Unlock()
	}
}

func TestSessionResourcesStableAcrossListGetAndChildren(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithTitle("Root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{sessionAgentAttribute: "root"}))
	childB := session.New(session.WithID("child-b"), session.WithParentID(root.ID), session.WithTitle("B"), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
	childA := session.New(session.WithID("child-a"), session.WithParentID(root.ID), session.WithTitle("A"), session.WithAgentName("worker"), session.WithAttributes(map[string]string{sessionAgentAttribute: "worker"}))
	for _, sess := range []*session.Session{root, childB, childA} {
		require.NoError(t, store.AddSession(t.Context(), sess))
	}
	srv := NewWithManager(NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(&httpSessionRegistry{sessions: map[string]*httpSession{}})), "")
	list := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions", "", "")
	var catalog sessionCatalogDTO
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &catalog))
	byID := map[string]sessionResourceDTO{}
	for _, resource := range catalog.Sessions {
		byID[resource.SessionID] = resource
	}
	get := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/root", "", "")
	var rootResource sessionResourceDTO
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &rootResource))
	assert.Equal(t, byID[root.ID].SessionID, rootResource.SessionID)
	assert.Equal(t, byID[root.ID].CreatedAt, rootResource.CreatedAt)
	assert.Equal(t, byID[root.ID].UpdatedAt, rootResource.UpdatedAt)
	children := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/root/children", "", "")
	assert.Equal(t, http.StatusNotFound, children.Code)
}

func TestPublicSessionRoutingFailuresHideInternalTerminology(t *testing.T) {
	for _, operation := range []string{"restore_binding", "restore_parent", "restore_tree_membership", "restore_ancestry", "restore_source", "restore_child_tree", "create_actor"} {
		err := sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: "s", Operation: runtime.SessionOperation(operation)})
		httpErr := func() *echo.HTTPError {
			target := &echo.HTTPError{}
			require.ErrorAs(t, err, &target)
			return target
		}()
		data, marshalErr := json.Marshal(httpErr.Message)
		require.NoError(t, marshalErr)
		body := string(data)
		assert.NotContains(t, body, "act"+"or")
		assert.NotContains(t, body, "restore")
		assert.Contains(t, body, `"session_id":"s"`)
	}
}

func TestSessionSnapshotPreservesTypedInputMetadata(t *testing.T) {
	for _, origin := range []session.InputOrigin{session.InputOriginUser, session.InputOriginAgent, session.InputOriginRuntime, "", "future"} {
		input := runtime.PendingInput{TurnID: "input", Content: "body", InputOrigin: origin, SenderID: "child", SenderName: "worker", InputMode: "steer", SessionPosition: 3}
		snapshot := sessionSnapshot(runtime.SessionSnapshot{PendingInputs: []runtime.PendingInput{input}})
		require.Len(t, snapshot.PendingInputs, 1)
		got := snapshot.PendingInputs[0]
		assert.Equal(t, origin, got.InputOrigin)
		assert.Equal(t, input.SenderID, got.SenderID)
		assert.Equal(t, input.SenderName, got.SenderName)
		assert.Equal(t, input.InputMode, got.InputMode)
		data, err := json.Marshal(got)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"sender_id":"child"`)
		assert.Contains(t, string(data), `"input_mode":"steer"`)
	}
}

func TestSessionHTTPEpochMismatchCarriesFreshBaseline(t *testing.T) {
	live := make(chan runtime.SessionEvent)
	close(live)
	sess := session.New(session.WithID("epoch-session"))
	sess.AddMessage(session.UserMessage("fresh transcript"))
	approval := &runtime.ToolCallConfirmationEvent{Type: "tool_call_confirmation", SessionID: sess.ID, RequestID: "fresh-approval"}
	handle := &httpSession{id: sess.ID, agent: "root", attach: runtime.Observation{
		Initial: []runtime.SessionSnapshot{{
			Epoch: "new-process", Cursor: 10, Session: sess,
			Status:       runtime.SessionStatus{SessionID: sess.ID, AgentName: "root", State: runtime.SessionStateRunning},
			Interactions: []runtime.InteractionSnapshot{{SessionID: sess.ID, InteractionID: "fresh-approval", Kind: runtime.InteractionConfirmation, Event: approval}},
		}},
		Replay: []runtime.SessionEvent{{Version: 1, SessionID: sess.ID, Epoch: "new-process", TranscriptPosition: -1, Event: runtime.AgentChoice("root", sess.ID, "fresh tail")}},
		Events: live, Cancel: func() {},
	}}
	server, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{sess.ID: handle}})
	httpServer := httptest.NewServer(server.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(sess.ID)
	require.NoError(t, err)
	cursor := uint64(5)
	observation, err := remote.Observe(t.Context(), runtime.ObserveOptions{Since: &cursor, SinceEpoch: "old-process"})
	require.NoError(t, err)
	defer observation.Cancel()
	assert.Equal(t, "new-process", observation.Primary().Epoch)
	assert.Equal(t, uint64(10), observation.Primary().Cursor)
	assert.Equal(t, "fresh transcript", observation.Primary().Session.Messages[0].Message.Message.Content)
	require.Len(t, observation.Primary().Interactions, 1)
	assert.Equal(t, "fresh-approval", observation.Primary().Interactions[0].InteractionID)
	require.Len(t, observation.Replay, 1)
	assert.True(t, observation.Replay[0].IsLiveSeed())
	assert.Equal(t, "new-process", observation.Replay[0].Epoch)
	handle.mu.Lock()
	defer handle.mu.Unlock()
	require.Len(t, handle.attachOptions, 1)
	assert.Equal(t, "old-process", handle.attachOptions[0].SinceEpoch)
	require.NotNil(t, handle.attachOptions[0].Since)
	assert.Equal(t, cursor, *handle.attachOptions[0].Since)
}

func (a *httpSession) StopSubtree(context.Context) error { a.stops++; return nil }
