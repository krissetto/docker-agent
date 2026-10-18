package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/host"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/teamedit"
)

type remoteTeamEditProvider struct {
	base.Config
	mu    sync.Mutex
	calls int
}

func (p *remoteTeamEditProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("openai/offline") }
func (*remoteTeamEditProvider) MaxTokens() int     { return 4096 }
func (p *remoteTeamEditProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	var name, arguments string
	switch p.calls {
	case 1:
		name, arguments = teamedit.ToolName, `{"action":"apply","agent_key":"root","instruction":"updated remote host edit"}`
	case 2:
		var revision string
		for _, message := range messages {
			if message.Role != chat.MessageRoleTool {
				continue
			}
			var receipt struct {
				Revision struct {
					ID string `json:"id"`
				} `json:"Revision"`
			}
			if json.Unmarshal([]byte(message.Content), &receipt) == nil {
				revision = receipt.Revision.ID
			}
		}
		if revision == "" {
			return nil, io.ErrUnexpectedEOF
		}
		name = teamedit.PersistToolName
		encoded, _ := json.Marshal(map[string]string{"revision": revision})
		arguments = string(encoded)
	default:
		return &remoteTeamEditStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "done"}}}}, {Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}}}}, nil
	}
	return &remoteTeamEditStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "remote-team-edit", Type: "function", Function: tools.FunctionCall{Name: name, Arguments: arguments}}}}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}, Usage: &chat.Usage{InputTokens: 2, OutputTokens: 2}},
	}}, nil
}

type remoteTeamEditStream struct {
	responses []chat.MessageStreamResponse
	index     int
}

func (s *remoteTeamEditStream) Recv() (chat.MessageStreamResponse, error) {
	if s.index >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	value := s.responses[s.index]
	s.index++
	return value, nil
}
func (*remoteTeamEditStream) Close() {}

func TestRemoteSessionTransportTeamEditApplyThenPersist(t *testing.T) {
	workspace := t.TempDir()
	sourcePath := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(sourcePath, []byte(`version: "16"
id: remote-team-edit
agents:
  root:
    id: root-stable
    model: openai/offline
    instruction: before
    toolsets:
      - type: team_edit
`), 0o600))
	model := &remoteTeamEditProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "offline"}}}
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
		return model, nil
	}})
	catalog, err := modelsdev.NewStore(modelsdev.WithCache(filepath.Join(t.TempDir(), "catalog.json")), modelsdev.WithKnownProvider(func(string) bool { return false }))
	require.NoError(t, err)
	cfg := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace, TeamEditMode: "restricted"}, ModelsDevStoreOverride: catalog, EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=fake-provider-only"})}
	owner, err := host.NewSessionRuntime(t.Context(), config.NewFileSource(sourcePath), cfg, session.NewInMemorySessionStore(), host.RuntimeOptions{WorkingDir: workspace, LoaderOptions: []teamloader.Opt{teamloader.WithProviderRegistry(registry), teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{teamedit.ToolsetType: teamedit.Creator}))}})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }()
	apiServer, err := server.New(t.Context(), session.NewInMemorySessionStore(), &config.RuntimeConfig{}, 0, nil, "", 1<<20, server.WithSessionRuntime(owner.Runtime()))
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveCtx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = apiServer.Serve(serveCtx, listener) }()
	defer listener.Close()
	client, err := runtime.NewClient("http://" + listener.Addr().String())
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.CreateSession(t.Context(), session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	first, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "edit team"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), first.TurnID))
	second, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "persist team"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), second.TurnID))
	persisted, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	require.Contains(t, string(persisted), "updated remote host edit")
}
