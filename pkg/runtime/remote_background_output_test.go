package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRemoteBackgroundOutputLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		feature, body string
		want          error
	}{
		{name: "missing_endpoint", status: http.StatusNotFound, body: `{"message":"Not Found"}`, want: tools.ErrBackgroundProcessOutputUnsupported},
		{name: "unsupported_endpoint", status: http.StatusNotImplemented, want: tools.ErrBackgroundProcessOutputUnsupported},
		{name: "missing_feature", status: http.StatusOK, body: `{}`, want: tools.ErrBackgroundProcessOutputUnsupported},
		{name: "unsupported", status: http.StatusNotImplemented, body: `{"error":"background_process_output_unsupported"}`, want: tools.ErrBackgroundProcessOutputUnsupported},
		{name: "not_found", status: http.StatusNotFound, feature: api.SessionFeatureBackgroundProcessOutput, body: `{"error":"background_process_not_found"}`, want: tools.ErrBackgroundProcessNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, api.SessionAPIPath+"/s/background-processes/job_1_1/output", r.URL.Path)
				assert.Equal(t, "32", r.URL.Query().Get("max_bytes"))
				assert.Contains(t, r.Header.Get(api.SessionFeaturesHeader), api.SessionFeatureBackgroundProcessOutput)
				w.Header().Set(api.SessionFeaturesHeader, tc.feature)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("s")
			require.NoError(t, err)
			_, err = handle.(tools.BackgroundProcessOutputSource).BackgroundProcessOutput(t.Context(), "job_1_1", 32)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestRemoteBackgroundOutputEnvelopeValidation(t *testing.T) {
	client, err := NewClient("http://unused.invalid")
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		mutate func(*remoteSessionEnvelope)
	}{
		{"durable_sequence", func(e *remoteSessionEnvelope) { e.Sequence = 1 }},
		{"turn", func(e *remoteSessionEnvelope) { e.TurnID = "turn" }},
		{"interaction", func(e *remoteSessionEnvelope) { e.InteractionID = "interaction" }},
		{"gap", func(e *remoteSessionEnvelope) { e.Gap = true }},
		{"position", func(e *remoteSessionEnvelope) { e.TranscriptPosition = 0 }},
		{"wrong_session", func(e *remoteSessionEnvelope) { e.SessionID = "other" }},
		{"negative_offset", func(e *remoteSessionEnvelope) {
			e.Event = json.RawMessage(`{"type":"background_process_output","session_id":"s","process_id":"job_1_1","tool_call_id":"call","offset":-1}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := remoteSessionEnvelope{Version: api.SessionAPIVersion, SessionID: "s", TranscriptPosition: -1, Event: json.RawMessage(`{"type":"background_process_output","session_id":"s","process_id":"job_1_1","tool_call_id":"call","offset":0,"output":"hello"}`)}
			tc.mutate(&envelope)
			_, err := client.decodeSessionEnvelope(envelope)
			require.Error(t, err)
		})
	}
}

func TestRemoteBackgroundOutputRejectReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"version":2,"type":"snapshot","snapshot":{"session":{"id":"s"},"status":{"session_id":"s","state":"running"},"cursor":4}}

data: {"version":2,"type":"event","envelope":{"version":2,"session_id":"s","sequence":0,"transcript_position":-1,"event":{"type":"background_process_output","session_id":"s","process_id":"job_1_1","tool_call_id":"call","offset":0,"output":"not replayable"}}}

data: {"version":2,"type":"ready","cursor":4}

`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	_, err = handle.Observe(t.Context(), ObserveOptions{})
	require.ErrorContains(t, err, "cannot be replayed")
}

func TestRemoteBackgroundOutputInterestLegacyFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get(api.SessionObserverHeader))
		assert.JSONEq(t, `{"s":["job"]}`, r.Header.Get(api.SessionBackgroundOutputInterestHeader))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"}}}\n\ndata: {\"version\":2,\"type\":\"ready\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	observation, err := client.attachSession(t.Context(), "s", ObserveOptions{BackgroundOutputInterest: map[string][]string{"s": {"job"}}})
	require.NoError(t, err)
	defer observation.Cancel()
	require.NotNil(t, observation.SetBackgroundOutputInterest)
	require.ErrorIs(t, observation.SetBackgroundOutputInterest("s", []string{"job"}), tools.ErrBackgroundProcessOutputUnsupported)
}
