package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestRemoteStartupLeavesPresentationToSessionObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("startup must not fetch source-wide agent configuration")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	services, err := NewRemoteServices(client, WithRemoteCurrentAgent("root"))
	require.NoError(t, err)
	for _, name := range []string{"root", "worker"} {
		services.EmitStartupInfo(context.Background(), session.New(session.WithAgentName(name)), EventSinkFunc(func(Event) {
			t.Error("startup must not overwrite canonical session presentation")
		}))
	}
}

func TestRemoteIdleObservationRetainsCanonicalModelReasoningAndAgent(t *testing.T) {
	cfg := latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}
	worker := agent.New("worker", "", agent.WithModel(newConfigProvider(cfg)))
	owner, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(newConfigProvider(cfg))), worker)), WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	defer owner.Close()
	handle, err := owner.CreateSession(t.Context(), session.New(session.WithID("idle"), session.WithAgentName("worker")), SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	initial, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer initial.Cancel()
	baseline := initial.Primary()
	require.Equal(t, SessionStateSettled, baseline.Status.State)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, api.SessionAPIPath+"/idle/events", r.URL.Path)
		snapshot := api.SessionSnapshot[SessionState, InteractionKind, Event]{Session: baseline.Session, Status: api.SessionStatus[SessionState](baseline.Status), Presentation: baseline.Presentation, TitleStatus: "failed"}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, message := range []any{
			api.SessionStreamMessage[SessionState, InteractionKind, Event]{Version: 2, Type: "snapshot", Snapshot: &snapshot},
			api.SessionStreamMessage[SessionState, InteractionKind, Event]{Version: 2, Type: "ready"},
		} {
			data, err := json.Marshal(message)
			require.NoError(t, err)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID("idle")
	require.NoError(t, err)
	for range 2 {
		observed, err := remote.Observe(t.Context(), ObserveOptions{})
		require.NoError(t, err)
		snapshot := observed.Primary()
		require.Equal(t, "worker", snapshot.Status.AgentName)
		require.Equal(t, "failed", snapshot.TitleStatus)
		require.Len(t, snapshot.Presentation, 2)
		info := snapshot.Presentation[0].(*AgentInfoEvent)
		require.Equal(t, "openai/gpt-5", info.Model)
		roster := snapshot.Presentation[1].(*TeamInfoEvent)
		require.Equal(t, "worker", roster.CurrentAgent)
		found := false
		for _, detail := range roster.AvailableAgents {
			if detail.Name == "worker" {
				found = true
				require.Equal(t, "gpt-5", detail.ModelID)
				require.NotEmpty(t, detail.ModelName)
				require.Equal(t, "effort", detail.ThinkingMode)
				require.Equal(t, "high", detail.ThinkingLevel)
			}
		}
		require.True(t, found)
		observed.Cancel()
	}
}
