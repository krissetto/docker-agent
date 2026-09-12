package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionTransportCanonicalCommandsAndReplay(t *testing.T) {
	var mu sync.Mutex
	var submitted []string
	var createSource string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			var request struct {
				Source string `json:"source"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			createSource = request.Source
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"session_id":"remote-1","agent_name":"root","capabilities":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/remote-1/messages":
			var request struct {
				Content string `json:"content"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			submitted = append(submitted, request.Content)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"session_id":"remote-1","turn_id":"turn-1","disposition":"queued"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/remote-1/events":
			assert.Equal(t, "7", r.URL.Query().Get("since"))
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			writer := bufio.NewWriter(w)
			frames := []string{
				`{"version":1,"type":"snapshot","snapshot":{"session":{"id":"remote-1"},"status":{"session_id":"remote-1","agent_name":"root","state":"settled","pending":0},"interactions":[],"pending_inputs":[],"cursor":8,"transcript_position":0}}`,
				`{"version":1,"type":"event","envelope":{"version":1,"session_id":"remote-1","turn_id":"turn-old","sequence":8,"transcript_position":-1,"event":{"type":"stream_started","session_id":"remote-1","agent_name":"root"}}}`,
				`{"version":1,"type":"ready","cursor":8}`,
				`{"version":1,"type":"event","envelope":{"version":1,"session_id":"remote-1","turn_id":"turn-1","sequence":9,"transcript_position":-1,"event":{"type":"stream_stopped","session_id":"remote-1","agent_name":"root","reason":"normal"}}}`,
			}
			for _, frame := range frames {
				fmt.Fprintf(writer, "data: %s\n\n", frame)
				if !assert.NoError(t, writer.Flush()) {
					return
				}
				flusher.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	sessions, err := NewSessionTransport(client, WithSessionTransportSource("second.yaml"))
	require.NoError(t, err)
	handle, err := sessions.CreateSession(t.Context(), session.New(session.WithID("client-placeholder")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Equal(t, "remote-1", handle.ID())
	assert.Equal(t, "second.yaml", createSource)

	submission, err := handle.Submit(t.Context(), TurnInput{Content: "hello"})
	require.NoError(t, err)
	assert.Equal(t, "turn-1", submission.TurnID)
	assert.Equal(t, SubmissionDispositionQueued, submission.Disposition)

	since := uint64(7)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Since: &since})
	require.NoError(t, err)
	defer observation.Cancel()
	assert.Equal(t, uint64(8), observation.Primary().Cursor)
	assert.Nil(t, observation.SessionsAdded)
	require.Len(t, observation.Replay, 1)
	assert.Equal(t, uint64(8), observation.Replay[0].Sequence)
	select {
	case envelope := <-observation.Events:
		assert.Equal(t, uint64(9), envelope.Sequence)
		assert.Equal(t, "turn-1", envelope.TurnID)
	case <-t.Context().Done():
		t.Fatal("timed out waiting for live session envelope")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"hello"}, submitted)
}

func TestRemoteSessionConcurrentMetadataAccessIsIsolated(t *testing.T) {
	const metadata = `{"session_id":"s","agent_name":"root","model":"test/fast","thinking_levels":["low","high"],"thinking_level":"low","capabilities":{"model_switching":true,"thinking_levels":true,"available_models":["test/fast","test/slow"]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/s/status":
			fmt.Fprintf(w, `{"metadata":%s,"status":{"session_id":"s","agent_name":"root","state":"settled","pending":0}}`, metadata)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/sessions/s/model":
			fmt.Fprint(w, metadata)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/s/thinking-level":
			fmt.Fprintf(w, `{"levels":["low","high"],"current":"low","metadata":%s}`, metadata)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	remote := handle.(*remoteSession)
	_, err = remote.Status(t.Context())
	require.NoError(t, err)

	const iterations = 25
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for range iterations {
			assert.Equal(t, "s", handle.ID())
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			got := handle.Metadata()
			got.ThinkingLevels[0] = "mutated"
			got.Capabilities.AvailableModels[0] = "mutated"
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			_, statusErr := handle.Status(t.Context())
			assert.NoError(t, statusErr)
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			assert.NoError(t, remote.SetModel(t.Context(), "test/fast"))
		}
	}()
	wg.Wait()

	got := handle.Metadata()
	assert.Equal(t, "s", handle.ID())
	assert.Equal(t, "low", string(got.ThinkingLevels[0]))
	assert.Equal(t, "test/fast", got.Capabilities.AvailableModels[0])
}

func TestSessionTransportRejectsUnknownEventType(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"version\":1,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"state\":\"settled\",\"pending\":0},\"interactions\":[],\"pending_inputs\":[],\"cursor\":1,\"transcript_position\":0}}\n\n")
		fmt.Fprint(w, "data: {\"version\":1,\"type\":\"event\",\"envelope\":{\"version\":1,\"session_id\":\"s\",\"sequence\":1,\"event\":{\"type\":\"future_unregistered_event\"}}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	sessions, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := sessions.SessionByID("s")
	require.NoError(t, err)
	_, err = handle.Observe(t.Context(), ObserveOptions{})
	require.ErrorContains(t, err, "unknown session event type")
}

func TestSessionTransportSingleSourceOmitsSource(t *testing.T) {
	var source *string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if raw, ok := request["source"]; ok {
			var value string
			if !assert.NoError(t, json.Unmarshal(raw, &value)) {
				http.Error(w, "invalid source", http.StatusBadRequest)
				return
			}
			source = &value
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"session_id":"single","agent_name":"root","capabilities":{}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	sessions, err := NewSessionTransport(client)
	require.NoError(t, err)
	_, err = sessions.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Nil(t, source)
}

func TestSessionTransportAllCommandsDeleteAndReconnect(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			fmt.Fprint(w, `{"session_id":"s","agent_name":"root","capabilities":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/s/messages":
			var body struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"session_id":"s","turn_id":%q}`, body.Mode+"-turn")
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/s/retry":
			fmt.Fprint(w, `{"session_id":"s","turn_id":"retry-turn","disposition":"queued"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/s/responses":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/sessions/s/title":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/s/cancel":
			fmt.Fprint(w, `{"session_id":"s","turn_id":"turn","outcome":"accepted"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/sessions/s":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/s/status":
			fmt.Fprint(w, `{"metadata":{"session_id":"s","agent_name":"root","capabilities":{}},"status":{"session_id":"s","agent_name":"root","state":"settled","pending":0}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/s/events":
			cursor := uint64(2)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"version\":1,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"state\":\"settled\",\"pending\":0},\"interactions\":[],\"pending_inputs\":[],\"cursor\":%d,\"transcript_position\":0}}\n\n", cursor)
			fmt.Fprintf(w, "data: {\"version\":1,\"type\":\"ready\",\"cursor\":%d}\n\n", cursor)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	for mode, call := range map[string]func(context.Context, TurnInput) (Submission, error){"submit": handle.Submit, "steer": handle.Steer} {
		submission, callErr := call(t.Context(), TurnInput{Content: mode})
		require.NoError(t, callErr)
		assert.Equal(t, mode+"-turn", submission.TurnID)
	}
	retry, err := handle.Retry(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, retry.Disposition)
	require.NoError(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: "i", Kind: InteractionConfirmation, Resume: ResumeRequest{Type: ResumeTypeApprove}}))
	require.NoError(t, handle.UpdateTitle(t.Context(), "title"))
	_, err = handle.Cancel(t.Context(), "turn")
	require.NoError(t, err)
	_, err = handle.Status(t.Context())
	require.NoError(t, err)
	first, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	first.Cancel()
	since := uint64(2)
	second, err := handle.Observe(t.Context(), ObserveOptions{Since: &since})
	require.NoError(t, err)
	second.Cancel()
	require.NoError(t, transport.DeleteSession(t.Context(), "s"))
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, requests, "GET /api/sessions/s/events?since=2")
	assert.Contains(t, requests, "DELETE /api/sessions/s")
}
