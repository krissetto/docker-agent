package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type outputHTTPSession struct {
	*httpSession
	output   tools.BackgroundProcessOutput
	maxBytes int
}

func (s *outputHTTPSession) BackgroundProcessOutput(_ context.Context, processID string, maxBytes int) (tools.BackgroundProcessOutput, error) {
	s.maxBytes = maxBytes
	if processID != s.output.ProcessID {
		return tools.BackgroundProcessOutput{}, tools.ErrBackgroundProcessNotFound
	}
	return s.output, nil
}

func TestSessionBackgroundOutputSnapshotRoundtrip(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	output := tools.BackgroundProcessOutput{SessionID: "s", ProcessID: "job_1_1", ToolCallID: "call", State: tools.BackgroundProcessRunning, Offset: 2, Output: "abcdef", Total: 8}
	source := &outputHTTPSession{httpSession: &httpSession{id: "s"}, output: output}
	other := &outputHTTPSession{httpSession: &httpSession{id: "other"}, output: output}
	registry.registerLive("s", source)
	registry.registerLive("other", other)
	srv, _ := newSessionHTTPServer(t, registry)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	reader := handle.(tools.BackgroundProcessOutputSource)
	got, err := reader.BackgroundProcessOutput(t.Context(), "job_1_1", 3)
	require.NoError(t, err)
	assert.Equal(t, "def", got.Output)
	assert.Equal(t, int64(5), got.Offset)
	assert.Equal(t, int64(8), got.Total)
	assert.Equal(t, 3, source.maxBytes)
	_, err = reader.BackgroundProcessOutput(t.Context(), "missing", 10)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
	otherHandle, err := transport.SessionByID("other")
	require.NoError(t, err)
	_, err = otherHandle.(tools.BackgroundProcessOutputSource).BackgroundProcessOutput(t.Context(), "job_1_1", 10)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
	_, err = reader.BackgroundProcessOutput(t.Context(), "job_1_1", 1<<30)
	require.NoError(t, err)
	assert.Equal(t, api.SessionBackgroundProcessOutputMaxBytes, source.maxBytes)
	_, err = reader.BackgroundProcessOutput(t.Context(), "job_1_1", 0)
	require.NoError(t, err)
	assert.Equal(t, api.SessionBackgroundProcessOutputMaxBytes, source.maxBytes)
	for _, tc := range []struct {
		feature, query string
		status         int
	}{
		{"", "", http.StatusNotImplemented},
		{api.SessionFeatureBackgroundProcesses, "", http.StatusNotImplemented},
		{api.SessionFeatureBackgroundProcessOutput, "?max_bytes=-1", http.StatusBadRequest},
		{api.SessionFeatureBackgroundProcessOutput, "?max_bytes=oops", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodGet, api.SessionAPIPath+"/s/background-processes/job_1_1/output"+tc.query, nil)
		req.Header.Set(api.SessionFeaturesHeader, tc.feature)
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		assert.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
}

func TestSessionBackgroundOutputLiveTransport(t *testing.T) {
	for _, feature := range []string{"", api.SessionFeatureBackgroundProcesses, api.SessionFeatureBackgroundProcessOutput} {
		t.Run("feature="+feature, func(t *testing.T) {
			output := &runtime.BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: "s", ProcessID: "job_1_1", ToolCallID: "call", Offset: 2, Output: "live bytes"}
			envelope := runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: "s", Epoch: "epoch", TranscriptPosition: -1, Event: output}
			events := make(chan runtime.SessionEvent, 1)
			events <- envelope
			close(events)
			handle := &httpSession{id: "s", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID("s")), Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateRunning}, Epoch: "epoch", Cursor: 4}}, Replay: []runtime.SessionEvent{envelope}, Events: events, SetBackgroundOutputInterest: func(string, []string) error { return nil }}}
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{"s": handle}}
			srv, _ := newSessionHTTPServer(t, registry)
			req := httptest.NewRequest(http.MethodGet, api.SessionAPIPath+"/s/events", nil)
			req.Header.Set(api.SessionFeaturesHeader, feature)
			if feature == api.SessionFeatureBackgroundProcessOutput {
				req.Header.Set(api.SessionObserverHeader, "no-interest")
			}
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			expected := 0
			assert.Equal(t, expected, strings.Count(rec.Body.String(), "live bytes"), "output must only appear live, never in replay")
		})
	}
	// The first-party remote reader accepts the zero-sequence live output without advancing its cursor.
	events := make(chan runtime.SessionEvent, 2)
	output := &runtime.BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: "s", ProcessID: "job_1_1", ToolCallID: "call", Output: "roundtrip"}
	events <- runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: "s", Epoch: "epoch", TranscriptPosition: -1, Event: output}
	events <- runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: "s", Epoch: "epoch", Sequence: 5, TranscriptPosition: -1, Event: runtime.StreamStopped("s", "root", "normal")}
	close(events)
	handle := &httpSession{id: "s", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID("s")), Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateRunning}, Epoch: "epoch", Cursor: 4}}, Events: events, SetBackgroundOutputInterest: func(string, []string) error { return nil }}}
	srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{"s": handle}})
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID("s")
	require.NoError(t, err)
	observation, err := remote.Observe(t.Context(), runtime.ObserveOptions{BackgroundOutputInterest: map[string][]string{"s": {"job_1_1"}}})
	require.NoError(t, err)
	defer observation.Cancel()
	assert.Empty(t, observation.Replay)
	got, ok := <-observation.Events
	require.True(t, ok)
	assert.Zero(t, got.Sequence)
	assert.Equal(t, output, got.Event)
	got, ok = <-observation.Events
	require.True(t, ok)
	assert.Equal(t, uint64(5), got.Sequence)
}

func TestSessionBackgroundOutputNeverSeeds(t *testing.T) {
	envelope := runtime.SessionEvent{Event: &runtime.BackgroundProcessOutputEvent{Type: "background_process_output"}}
	assert.Nil(t, sessionEnvelopeForRequest(envelope, true, false))
	dto := sessionEnvelopeForRequest(envelope, true, true)
	require.NotNil(t, dto)
	_, err := json.Marshal(dto)
	require.NoError(t, err)
}

type interestHTTPStream struct {
	events  chan runtime.SessionEvent
	initial map[string][]string
}

type interestHTTPSession struct {
	*httpSession
	streams chan interestHTTPStream
}

func (s *interestHTTPSession) Observe(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	events := make(chan runtime.SessionEvent, 16)
	s.streams <- interestHTTPStream{events: events, initial: options.BackgroundOutputInterest}
	return runtime.Observation{
		Initial: []runtime.SessionSnapshot{{Session: session.New(session.WithID(s.id)), Status: runtime.SessionStatus{SessionID: s.id, State: runtime.SessionStateRunning}, Epoch: "epoch"}},
		Events:  events,
		Cancel:  func() {},
		SetBackgroundOutputInterest: func(id string, _ []string) error {
			if id != s.id {
				return &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
			}
			return nil
		},
	}, nil
}

func TestSessionBackgroundOutputObservationInterest(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	local := &interestHTTPSession{httpSession: &httpSession{id: "s"}, streams: make(chan interestHTTPStream, 8)}
	registry.registerLive("s", local)
	srv, _ := newSessionHTTPServer(t, registry)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first, err := handle.Observe(ctx, runtime.ObserveOptions{BackgroundOutputInterest: map[string][]string{"s": {"a"}}})
	require.NoError(t, err)
	defer first.Cancel()
	firstStream := <-local.streams
	require.Equal(t, map[string][]string{"s": {"a"}}, firstStream.initial)
	second, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer second.Cancel()
	secondStream := <-local.streams
	require.NoError(t, second.SetBackgroundOutputInterest("s", []string{"b"}))
	require.Error(t, first.SetBackgroundOutputInterest("other", []string{"a"}))
	publish := func(stream interestHTTPStream, sequence uint64) {
		for _, id := range []string{"a", "b"} {
			stream.events <- runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: "s", Epoch: "epoch", TranscriptPosition: -1, Event: &runtime.BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: "s", ProcessID: id, ToolCallID: "call", Output: "bytes for " + id}}
		}
		stream.events <- runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: "s", Epoch: "epoch", Sequence: sequence, TranscriptPosition: -1, Event: runtime.StreamStopped("s", "root", "barrier")}
	}
	expect := func(observation runtime.Observation, processID string) {
		t.Helper()
		if processID != "" {
			select {
			case event := <-observation.Events:
				output, ok := event.Event.(*runtime.BackgroundProcessOutputEvent)
				require.True(t, ok, "expected output, got %T", event.Event)
				require.Equal(t, processID, output.ProcessID)
			case <-ctx.Done():
				t.Fatal("timed out waiting for output")
			}
		}
		select {
		case event := <-observation.Events:
			require.IsType(t, &runtime.StreamStoppedEvent{}, event.Event, "uninterested output crossed the stream")
		case <-ctx.Done():
			t.Fatal("timed out waiting for barrier")
		}
	}
	publish(firstStream, 1)
	publish(secondStream, 1)
	expect(first, "a")
	expect(second, "b")
	require.NoError(t, first.SetBackgroundOutputInterest("s", nil))
	publish(firstStream, 2)
	expect(first, "")
	publish(secondStream, 2)
	expect(second, "b")
	first.Cancel()
	require.ErrorIs(t, first.SetBackgroundOutputInterest("s", []string{"a"}), runtime.ErrSessionClosed)
	reconnected, err := handle.Observe(ctx, runtime.ObserveOptions{BackgroundOutputInterest: map[string][]string{"s": {"b"}}})
	require.NoError(t, err)
	defer reconnected.Cancel()
	reconnectStream := <-local.streams
	publish(reconnectStream, 1)
	expect(reconnected, "b")
}

func TestSessionBackgroundOutputInterestIsolationAndLimits(t *testing.T) {
	srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{}})
	calls := 0
	observer, unregister, err := srv.registerBackgroundOutputObserver("s", "observer", map[string][]string{}, runtime.Observation{SetBackgroundOutputInterest: func(id string, _ []string) error {
		calls++
		if id != "s" {
			return &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
		}
		return nil
	}})
	require.NoError(t, err)
	require.NotNil(t, observer)
	defer unregister()
	for _, tc := range []struct {
		root, body, feature string
		status              int
	}{
		{"other", `{"observer_id":"observer","session_id":"s","process_ids":["a"]}`, api.SessionFeatureBackgroundProcessOutput, http.StatusNotFound},
		{"s", `{"observer_id":"observer","session_id":"other","process_ids":["a"]}`, api.SessionFeatureBackgroundProcessOutput, http.StatusNotFound},
		{"s", `{"observer_id":"observer","session_id":"s","process_ids":[""]}`, api.SessionFeatureBackgroundProcessOutput, http.StatusBadRequest},
		{"s", `{"observer_id":"observer","session_id":"s","process_ids":["a"]}`, "", http.StatusNotImplemented},
	} {
		req := httptest.NewRequest(http.MethodPut, api.SessionAPIPath+"/"+tc.root+"/background-output-interest", strings.NewReader(tc.body))
		req.Header.Set(api.SessionFeaturesHeader, tc.feature)
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
	require.Equal(t, 1, calls, "foreign observer must never reach the observation setter")
	require.False(t, validBackgroundOutputInterest(map[string][]string{"s": make([]string, api.SessionBackgroundOutputInterestMaxProcesses+1)}))
	require.False(t, validBackgroundOutputInterest(map[string][]string{"s": {strings.Repeat("a", api.SessionBackgroundOutputInterestMaxBytes)}}))
	for _, raw := range []string{`{"s":[""]}`, `{"s":"invalid"}`, `{"s":["a"]} trailing`} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(api.SessionFeaturesHeader, api.SessionFeatureBackgroundProcessOutput)
		req.Header.Set(api.SessionObserverHeader, "observer")
		req.Header.Set(api.SessionBackgroundOutputInterestHeader, raw)
		_, _, err := parseBackgroundOutputObserver(req)
		require.Error(t, err)
	}
}
