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

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionTransportCanonicalCommandsAndReplay(t *testing.T) {
	var mu sync.Mutex
	var submitted []string
	var createSource string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath:
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
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath+"/remote-1/messages":
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
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/remote-1/events":
			assert.Equal(t, "7", r.URL.Query().Get("since"))
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			writer := bufio.NewWriter(w)
			frames := []string{
				`{"version":2,"type":"snapshot","snapshot":{"session":{"id":"remote-1"},"status":{"session_id":"remote-1","agent_name":"root","state":"settled","pending":0},"interactions":[],"pending_inputs":[],"cursor":8,"transcript_position":0}}`,
				`{"version":2,"type":"event","envelope":{"version":2,"session_id":"remote-1","turn_id":"turn-old","sequence":8,"transcript_position":-1,"event":{"type":"stream_started","session_id":"remote-1","agent_name":"root"}}}`,
				`{"version":2,"type":"ready","cursor":8}`,
				`{"version":2,"type":"event","envelope":{"version":2,"session_id":"remote-1","turn_id":"turn-1","sequence":9,"transcript_position":-1,"event":{"type":"stream_stopped","session_id":"remote-1","agent_name":"root","reason":"normal"}}}`,
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
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/s/status":
			fmt.Fprintf(w, `{"metadata":%s,"status":{"session_id":"s","agent_name":"root","state":"settled","pending":0}}`, metadata)
		case r.Method == http.MethodPatch && r.URL.Path == api.SessionAPIPath+"/s/model":
			fmt.Fprint(w, metadata)
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/s/thinking-level":
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
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"state\":\"settled\",\"pending\":0},\"interactions\":[],\"pending_inputs\":[],\"cursor\":1,\"transcript_position\":0}}\n\n")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"sequence\":1,\"event\":{\"type\":\"future_unregistered_event\"}}}\n\n")
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
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath:
			fmt.Fprint(w, `{"session_id":"s","agent_name":"root","capabilities":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath+"/s/messages":
			var body struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"session_id":"s","turn_id":%q}`, body.Mode+"-turn")
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath+"/s/retry":
			fmt.Fprint(w, `{"session_id":"s","turn_id":"retry-turn","disposition":"queued"}`)
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath+"/s/responses":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == api.SessionAPIPath+"/s/title":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath+"/s/cancel":
			fmt.Fprint(w, `{"session_id":"s","turn_id":"turn","outcome":"accepted"}`)
		case r.Method == http.MethodDelete && r.URL.Path == api.SessionAPIPath+"/s":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/s/status":
			fmt.Fprint(w, `{"metadata":{"session_id":"s","agent_name":"root","capabilities":{}},"status":{"session_id":"s","agent_name":"root","state":"settled","pending":0}}`)
		case r.Method == http.MethodGet && r.URL.Path == api.SessionAPIPath+"/s/events":
			cursor := uint64(2)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"state\":\"settled\",\"pending\":0},\"interactions\":[],\"pending_inputs\":[],\"cursor\":%d,\"transcript_position\":0}}\n\n", cursor)
			fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":%d}\n\n", cursor)
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
	assert.Contains(t, requests, "GET "+api.SessionAPIPath+"/s/events?since=2")
	assert.Contains(t, requests, "DELETE "+api.SessionAPIPath+"/s")
}

func TestRemoteSnapshotPreservesTypedInputMetadata(t *testing.T) {
	for _, origin := range []session.InputOrigin{session.InputOriginUser, session.InputOriginAgent, session.InputOriginRuntime, "", "future"} {
		var wire remoteSessionSnapshot
		data := fmt.Sprintf(`{"session":{"id":"s"},"status":{"session_id":"s"},"pending_inputs":[{"turn_id":"input","content":"body","input_origin":%q,"sender_id":"child","sender_name":"worker","input_mode":"steer","session_position":3}]}`, origin)
		require.NoError(t, json.Unmarshal([]byte(data), &wire))
		snapshot, err := (&Client{}).decodeSessionSnapshot(wire)
		require.NoError(t, err)
		require.Len(t, snapshot.PendingInputs, 1)
		assert.Equal(t, PendingInput{TurnID: "input", Content: "body", InputOrigin: origin, SenderID: "child", SenderName: "worker", InputMode: "steer", SessionPosition: 3}, snapshot.PendingInputs[0])
	}
}

func TestRemoteInputEventsPreserveTypedMetadataAndInputIdentity(t *testing.T) {
	c := &Client{registry: map[string]func() Event{
		"user_message":                  func() Event { return &UserMessageEvent{} },
		"pending_user_message_accepted": func() Event { return &PendingUserMessageAcceptedEvent{} },
		"pending_user_message_promoted": func() Event { return &PendingUserMessagePromotedEvent{} },
	}}
	for _, kind := range []string{"user_message", "pending_user_message_accepted", "pending_user_message_promoted"} {
		raw := fmt.Sprintf(`{"type":%q,"message":"body","turn_id":"accepted","input_origin":"agent","sender_id":"child","sender_name":"worker","input_mode":"steer"}`, kind)
		event, err := c.decodeSessionEvent([]byte(raw))
		require.NoError(t, err)
		data, err := json.Marshal(event)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"turn_id":"accepted"`)
		assert.Contains(t, string(data), `"input_origin":"agent"`)
		assert.Contains(t, string(data), `"sender_id":"child"`)
		assert.Contains(t, string(data), `"sender_name":"worker"`)
		assert.Contains(t, string(data), `"input_mode":"steer"`)
	}
}

func TestSessionSummaryCatalogRemoteRejectsLegacyAndInvalidIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{name: "legacy", payload: `{"version":2,"sessions":[{"session_id":"root","messages":[{"content":"legacy transcript"}]}]}`},
		{name: "duplicate", payload: `{"version":2,"view":"summary","sessions":[{"session_id":"same"},{"session_id":"same"}]}`},
		{name: "missing", payload: `{"version":2,"view":"summary","sessions":[{}]}`},
		{name: "wrong-scope", payload: `{"version":2,"view":"summary","sessions":[{"session_id":"child","parent_id":"root"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, api.SessionAPIPath, r.URL.Path)
				assert.Equal(t, "summary", r.URL.Query().Get("view"))
				assert.Equal(t, "false", r.URL.Query().Get("include_children"))
				fmt.Fprint(w, tc.payload)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			_, err = transport.ListSessionSummaries(t.Context(), SessionSummaryOptions{})
			require.Error(t, err)
			assert.Equal(t, 1, calls, "no transcript or per-ID compatibility fallback")
		})
	}
}

func TestSessionTransportRejectsNonCanonicalWireVersions(t *testing.T) {
	for _, version := range []int{0, 1, api.SessionAPIVersion + 1} {
		for _, boundary := range []string{"snapshot", "envelope"} {
			t.Run(fmt.Sprintf("%s/version=%d", boundary, version), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, api.SessionAPIPath+"/s/events", r.URL.Path)
					w.Header().Set("Content-Type", "text/event-stream")
					snapshotVersion := api.SessionAPIVersion
					if boundary == "snapshot" {
						snapshotVersion = version
					}
					fmt.Fprintf(w, "data: {\"version\":%d,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"cursor\":0}}\n\n", snapshotVersion)
					if boundary == "envelope" {
						fmt.Fprintf(w, "data: {\"version\":%d,\"type\":\"event\",\"envelope\":{\"version\":%d,\"session_id\":\"s\",\"sequence\":1,\"event\":{\"type\":\"stream_started\",\"session_id\":\"s\"}}}\n\n", api.SessionAPIVersion, version)
					}
				}))
				defer server.Close()
				client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
				require.NoError(t, err)
				transport, err := NewSessionTransport(client)
				require.NoError(t, err)
				handle, err := transport.SessionByID("s")
				require.NoError(t, err)
				_, err = handle.Observe(t.Context(), ObserveOptions{})
				require.ErrorContains(t, err, "version")
			})
		}
	}
}
