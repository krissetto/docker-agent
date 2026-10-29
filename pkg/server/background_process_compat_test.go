package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestBackgroundProcessTransportNegotiation(t *testing.T) {
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v2/sessions/s/events", http.NoBody)
	process := tools.BackgroundProcess{ID: "job", SessionID: "s", StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
	lifecycle := runtime.SessionEvent{Version: 1, SessionID: "s", Epoch: "epoch", Sequence: 4, TranscriptPosition: -1, Event: &runtime.BackgroundProcessesEvent{Type: "background_processes", SessionID: "s", Processes: []tools.BackgroundProcess{process}}}
	legacy := sessionEnvelopeForRequest(lifecycle, false)
	require.NotNil(t, legacy)
	require.Nil(t, legacy.Event, "legacy clients retain the sequence but do not receive an unknown event")
	require.Equal(t, uint64(4), legacy.Sequence)
	seed := lifecycle
	seed.Sequence = 0
	seed.TurnID = ""
	seed.InteractionID = ""
	seed.TranscriptPosition = -1
	require.Nil(t, sessionEnvelopeForRequest(seed, false), "legacy clients must not receive unknown sequence-zero seeds")
	request.Header.Set(api.SessionFeaturesHeader, "streaming, "+api.SessionFeatureBackgroundProcesses)
	require.True(t, supportsBackgroundProcessFeature(request))
	negotiated := sessionEnvelopeForRequest(lifecycle, supportsBackgroundProcessFeature(request))
	require.NotNil(t, negotiated)
	require.NotNil(t, negotiated.Event)
	featureContext := echo.New().NewContext(request, httptest.NewRecorder())
	snapshot := sessionSnapshotForRequest(runtime.SessionSnapshot{BackgroundProcesses: []tools.BackgroundProcess{process}}, featureContext)
	require.Len(t, snapshot.BackgroundProcesses, 1)
	request.Header.Set(api.SessionFeaturesHeader, "background-processes"+string(make([]byte, 129)))
	require.False(t, supportsBackgroundProcessFeature(request), "unbounded feature values are ignored")
}

func TestLegacyAPIDecoderKeepsPayloadlessEventSequence(t *testing.T) {
	data, err := json.Marshal(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "event", Envelope: sessionEnvelopeForRequest(runtime.SessionEvent{Version: 1, SessionID: "s", Epoch: "e", Sequence: 17, TranscriptPosition: -1, Event: &runtime.BackgroundProcessesEvent{Type: "background_processes", SessionID: "s", Processes: []tools.BackgroundProcess{}}}, false)})
	require.NoError(t, err)
	var legacy struct {
		Envelope struct {
			SessionID string          `json:"session_id"`
			Sequence  uint64          `json:"sequence"`
			Event     json.RawMessage `json:"event"`
		} `json:"envelope"`
	}
	require.NoError(t, json.Unmarshal(data, &legacy))
	require.Equal(t, uint64(17), legacy.Envelope.Sequence)
	require.Empty(t, legacy.Envelope.Event)
}

func TestBackgroundProcessEventStreamLegacyAndNegotiated(t *testing.T) {
	for _, negotiated := range []bool{false, true} {
		t.Run(fmt.Sprintf("negotiated=%t", negotiated), func(t *testing.T) {
			live := make(chan runtime.SessionEvent, 1)
			process := tools.BackgroundProcess{ID: "job", SessionID: "compat-s", StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
			live <- runtime.SessionEvent{Version: 1, SessionID: "compat-s", Epoch: "epoch", Sequence: 1, TranscriptPosition: -1, Event: &runtime.BackgroundProcessesEvent{Type: "background_processes", SessionID: "compat-s", Processes: []tools.BackgroundProcess{process}}}
			close(live)
			initial := runtime.SessionSnapshot{Session: session.New(session.WithID("compat-s")), Status: runtime.SessionStatus{SessionID: "compat-s", State: runtime.SessionStateSettled}, Epoch: "epoch", Cursor: 0, BackgroundProcesses: []tools.BackgroundProcess{process}}
			handle := &httpSession{id: "compat-s", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{initial}, Events: live, Cancel: func() {}}}
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{"compat-s": handle}}
			manager := NewSessionManager(t.Context(), config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
			srv := NewWithManager(manager, "")
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v2/sessions/compat-s/events", http.NoBody)
			if negotiated {
				request.Header.Set(api.SessionFeaturesHeader, api.SessionFeatureBackgroundProcesses)
			}
			recorder := httptest.NewRecorder()
			srv.e.ServeHTTP(recorder, request)
			require.Equal(t, 200, recorder.Code)
			var snapshot map[string]json.RawMessage
			for line := range strings.SplitSeq(recorder.Body.String(), "\n") {
				if strings.HasPrefix(line, "data: ") {
					var frame sessionStreamMessage
					require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame))
					if frame.Type == "snapshot" {
						raw, _ := json.Marshal(frame.Snapshot)
						require.NoError(t, json.Unmarshal(raw, &snapshot))
						break
					}
				}
			}
			if negotiated {
				require.Contains(t, string(snapshot["background_processes"]), "job")
			} else {
				require.NotContains(t, snapshot, "background_processes")
			}
			if negotiated {
				require.Contains(t, recorder.Body.String(), `"type":"background_processes"`)
			} else {
				require.Contains(t, recorder.Body.String(), `"sequence":1`)
				require.NotContains(t, recorder.Body.String(), `"type":"background_processes"`)
			}
		})
	}
}

func TestBackgroundProcessReconnectStreamLegacyAndNegotiated(t *testing.T) {
	for _, negotiated := range []bool{false, true} {
		t.Run(fmt.Sprintf("negotiated=%t", negotiated), func(t *testing.T) {
			process := tools.BackgroundProcess{ID: "job", SessionID: "compat-s", StartedAt: time.Now(), State: tools.BackgroundProcessRunning}
			lifecycle := runtime.SessionEvent{Version: 1, SessionID: "compat-s", Epoch: "epoch", Sequence: 4, TranscriptPosition: -1, Event: &runtime.BackgroundProcessesEvent{Type: "background_processes", SessionID: "compat-s", Processes: []tools.BackgroundProcess{process}}}
			initial := runtime.SessionSnapshot{Session: session.New(session.WithID("compat-s")), Status: runtime.SessionStatus{SessionID: "compat-s", State: runtime.SessionStateSettled}, Epoch: "epoch", Cursor: 4, BackgroundProcesses: []tools.BackgroundProcess{process}}
			live := make(chan runtime.SessionEvent)
			close(live)
			handle := &httpSession{id: "compat-s", agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{initial}, Replay: []runtime.SessionEvent{lifecycle}, Events: live, Cancel: func() {}}}
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{"compat-s": handle}}
			manager := NewSessionManager(t.Context(), config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
			srv := NewWithManager(manager, "")
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v2/sessions/compat-s/events?since=3&since_epoch=epoch", http.NoBody)
			if negotiated {
				request.Header.Set(api.SessionFeaturesHeader, api.SessionFeatureBackgroundProcesses)
			}
			recorder := httptest.NewRecorder()
			srv.e.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Contains(t, recorder.Body.String(), `"sequence":4`)
			if negotiated {
				require.Contains(t, recorder.Body.String(), `"type":"background_processes"`)
			} else {
				require.NotContains(t, recorder.Body.String(), `"type":"background_processes"`)
			}
		})
	}
}
