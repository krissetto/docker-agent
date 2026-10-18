package root

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tui"
)

func TestLocalReviewSessionFactoryCreatesHiddenStableExplicitAgent(t *testing.T) {
	ctx := t.Context()
	workingDir, sourceFile := t.TempDir(), filepath.Join(t.TempDir(), "team.yaml")
	require.NoError(t, os.WriteFile(sourceFile, []byte(`version: "2"
agents:
  root:
    model: openai/gpt-4o
    instruction: root
  reviewer:
    model: openai/gpt-4o
    instruction: reviewer
`), 0o600))
	source := config.NewFileSource(sourceFile)
	store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer store.Close()
	flags := &runExecFlags{sessionDB: filepath.Join(t.TempDir(), "sessions.db")}
	flags.runConfig = config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}, EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"})}
	initialResources, err := flags.buildOwnedSessionResourcesForAgent(ctx, source, store, workingDir, "root")
	require.NoError(t, err)
	initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(workingDir))
	backend := &localBackend{flags: flags, agentSource: source}
	require.NoError(t, flags.configureSessionViewHost(ctx, backend, initialResources.services, initialResources.sessions, initial, nil, initialResources.cleanup))
	factory := flags.newReviewSessionFactory(ctx, backend, initialResources.services, initialResources.sessions)
	require.NotNil(t, factory)
	agents, err := factory.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, agents, 2)
	assert.Equal(t, source.Name(), agents[1].Source)

	var first tui.ReviewSessionResult
	var request tui.ReviewSessionRequest
	for i := range 9 {
		request = tui.ReviewSessionRequest{SessionID: fmt.Sprintf("review-stable-target-%d", i), RequestID: fmt.Sprintf("review-stable-request-%d", i), AgentName: "reviewer", WorkingDir: workingDir, Source: source.Name(), Body: "literal /commands ! payload"}
		result, err := factory.CreateAndSubmit(ctx, request)
		require.NoError(t, err, "accepted session-owner capacity is not exhausted by resolved requests")
		require.True(t, result.Accepted)
		require.Equal(t, request.SessionID, result.SessionID)
		if i == 0 {
			first = result
		}
	}
	request = tui.ReviewSessionRequest{SessionID: "review-stable-target-0", RequestID: "review-stable-request-0", AgentName: "reviewer", WorkingDir: workingDir, Source: source.Name(), Body: "literal /commands ! payload"}
	retry, err := factory.CreateAndSubmit(ctx, request)
	require.NoError(t, err)
	require.Equal(t, first.TurnID, retry.TurnID)
	tabs, _ := flags.sessionViewHost.GetTabs()
	assert.Empty(t, tabs, "review target creation must not add or focus a tab")
	created, err := store.GetSession(ctx, request.SessionID)
	require.NoError(t, err)
	assert.Equal(t, request.Source, created.AttributesSnapshot()[sessionActorSourceAttribute])
	assert.Equal(t, "reviewer", created.AttributesSnapshot()[runtime.SessionAgentAttribute])
	found := false
	for _, message := range created.MessagesSnapshot() {
		if message.Message != nil && message.Message.Message.Content == request.Body {
			found = true
		}
	}
	assert.True(t, found, "first review body is admitted literally into the selected agent session")
	changed := request
	changed.Body = "redirected"
	_, err = factory.CreateAndSubmit(ctx, changed)
	require.Error(t, err, "request ID cannot be changed after admission")
	flags.sessionViewHost.Shutdown()
}

func TestRemoteReviewSessionFactoryUsesAtomicStartWithExactSourceAndIDs(t *testing.T) {
	var calls int
	var got api.SessionStartRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != api.SessionAPIPath+"/start" {
			http.NotFound(w, r)
			return
		}
		calls++
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"session_id":"remote-review-id","turn_id":"remote-turn"}`)
	}))
	defer server.Close()
	flags := &runExecFlags{remoteAddress: server.URL}
	factory := &reviewSessionFactory{flags: flags, remote: &remoteBackend{flags: flags, agentFileName: "team.yaml"}}
	request := tui.ReviewSessionRequest{SessionID: "remote-review-id", RequestID: "stable-remote-request", AgentName: "reviewer", WorkingDir: "/server/work", Source: "team.yaml", Body: "literal review body"}
	result, err := factory.CreateAndSubmit(t.Context(), request)
	require.NoError(t, err)
	require.True(t, result.Accepted)
	require.Equal(t, request.SessionID, result.SessionID)
	require.Equal(t, request.RequestID, got.Input.RequestID)
	require.Equal(t, request.Body, got.Input.Content)
	require.Equal(t, request.Source, got.Source)
	require.Equal(t, request.AgentName, got.AgentName)
	require.Equal(t, request.WorkingDir, got.WorkingDir)
	request.Source = "other.yaml"
	_, err = factory.CreateAndSubmit(t.Context(), request)
	require.Error(t, err)
	assert.Equal(t, 1, calls, "mismatched source must be rejected before transport")
}
