package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/teamedit"
)

type snapshotHostModel struct{ base.Config }

func (*snapshotHostModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("unexpected completion")
}

func TestManagedWorkspaceSnapshotsStartup(t *testing.T) {
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		return &snapshotHostModel{base.Config{ModelConfig: *cfg}}, nil
	}})
	source := config.NewBytesSource("team.yaml", []byte("agents:\n  root:\n    model: openai/offline\n    instruction: test\n"))
	catalog, err := modelsdev.NewStore(modelsdev.WithCache(filepath.Join(t.TempDir(), "catalog.json")), modelsdev.WithKnownProvider(func(string) bool { return false }), modelsdev.WithFetcher(func(context.Context, string) (*modelsdev.Database, string, error) {
		return nil, "", errors.New("unexpected catalog fetch")
	}))
	require.NoError(t, err)
	var disabledDigest string
	workspace := t.TempDir()
	for _, enabled := range []bool{false, true} {
		manifest, err := snapshot.Snapshot(t.Context(), source, snapshot.Options{Startup: snapshot.Startup{Workspace: workspace, SourceKey: "team.yaml", Snapshots: enabled}})
		require.NoError(t, err)
		digest, err := manifest.Digest()
		require.NoError(t, err)
		if !enabled {
			disabledDigest = digest
		} else {
			require.NotEqual(t, disabledDigest, digest)
		}
		// Consume only the frozen startup flag, just as the managed server factory does.
		startup := manifest.Startup()
		owner, err := NewSessionRuntime(t.Context(), manifest.FrozenSource(), &config.RuntimeConfig{ModelsDevStoreOverride: catalog, EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=fake-provider-only"})}, session.NewInMemorySessionStore(), RuntimeOptions{WorkingDir: startup.Workspace, Snapshots: startup.Snapshots, LoaderOptions: []teamloader.Opt{teamloader.WithProviderRegistry(registry), teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{teamedit.ToolsetType: teamedit.Creator}))}})
		require.NoError(t, err)
		handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithWorkingDir(startup.Workspace)), runtime.SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		require.Equal(t, enabled, handle.Metadata().Capabilities.Snapshots)
		if enabled {
			history, err := handle.(runtime.SessionWorkspaceSnapshots).WorkspaceSnapshots(t.Context())
			require.NoError(t, err)
			require.True(t, history.Enabled)
		}
		require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context())))
	}
}

func TestSessionRuntimeExposesGrantedTeamEditCapabilities(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "team.yaml")
	data := []byte(`version: "16"
id: host-team-edit
agents:
  root:
    id: root-stable
    model: openai/offline
    toolsets:
      - type: team_edit
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		return &snapshotHostModel{base.Config{ModelConfig: *cfg}}, nil
	}})
	catalog, err := modelsdev.NewStore(modelsdev.WithCache(filepath.Join(t.TempDir(), "catalog.json")), modelsdev.WithKnownProvider(func(string) bool { return false }))
	require.NoError(t, err)
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace, TeamEditMode: "restricted"}, ModelsDevStoreOverride: catalog, EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=fake-provider-only"})}
	owner, err := NewSessionRuntime(t.Context(), config.NewFileSource(path), runConfig, session.NewInMemorySessionStore(), RuntimeOptions{WorkingDir: workspace, LoaderOptions: []teamloader.Opt{teamloader.WithProviderRegistry(registry), teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{teamedit.ToolsetType: teamedit.Creator}))}})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }()
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithWorkingDir(workspace)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	info, err := handle.(runtime.SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	names := make([]string, 0, len(info.Tools))
	for _, tool := range info.Tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, "team_edit")
	require.Contains(t, names, "team_edit_persist")
	require.Contains(t, names, "team_edit_export")
}

type hostTeamEditProvider struct {
	base.Config
	mu       sync.Mutex
	calls    int
	offered  [][]string
	revision func() string
}

func (p *hostTeamEditProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, offered []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	var names []string
	for _, tool := range offered {
		names = append(names, tool.Name)
	}
	p.offered = append(p.offered, names)
	var toolName, args string
	if p.calls == 1 {
		toolName = teamedit.ToolName
		args = `{"action":"apply","agent_key":"root","instruction":"updated host edit"}`
	} else if p.calls == 2 {
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
			if json.Unmarshal([]byte(message.Content), &receipt) == nil && receipt.Revision.ID != "" {
				revision = receipt.Revision.ID
			}
		}
		if revision == "" {
			return nil, fmt.Errorf("host team-edit test has no current revision: %v", messages)
		}
		toolName = teamedit.PersistToolName
		encoded, _ := json.Marshal(map[string]string{"revision": revision})
		args = string(encoded)
	} else {
		return &hostTeamEditStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "completed"}}}}, {Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}}}}, nil
	}
	return &hostTeamEditStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: fmt.Sprintf("team-edit-%d", p.calls), Type: "function", Function: tools.FunctionCall{Name: toolName, Arguments: args}}}}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}, Usage: &chat.Usage{InputTokens: 2, OutputTokens: 2}},
	}}, nil
}

func (p *hostTeamEditProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("openai/offline") }

func (*hostTeamEditProvider) MaxTokens() int { return 4096 }

type hostTeamEditStream struct {
	responses []chat.MessageStreamResponse
	index     int
}

func (s *hostTeamEditStream) Recv() (chat.MessageStreamResponse, error) {
	if s.index >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	response := s.responses[s.index]
	s.index++
	return response, nil
}
func (*hostTeamEditStream) Close() {}

func TestHostTeamEditPersistenceActualToolDispatch(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "team.yaml")
	data := []byte(`version: "16"
id: host-team-edit-call
agents:
  root:
    id: root-stable
    model: openai/offline
    instruction: before
    toolsets:
      - type: team_edit
`)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	model := &hostTeamEditProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "offline"}}}
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
		return model, nil
	}})
	catalog, err := modelsdev.NewStore(modelsdev.WithCache(filepath.Join(t.TempDir(), "catalog.json")), modelsdev.WithKnownProvider(func(string) bool { return false }))
	require.NoError(t, err)
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace, TeamEditMode: "restricted"}, ModelsDevStoreOverride: catalog, EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=fake-provider-only"})}
	owner, err := NewSessionRuntime(t.Context(), config.NewFileSource(path), runConfig, session.NewInMemorySessionStore(), RuntimeOptions{WorkingDir: workspace, LoaderOptions: []teamloader.Opt{teamloader.WithProviderRegistry(registry), teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{teamedit.ToolsetType: teamedit.Creator}))}})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }()
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("host-team-edit-call"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	editTurn, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "edit the team"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), editTurn.TurnID))
	persistTurn, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "persist the current revision"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), persistTurn.TurnID))
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.offered) < 2 {
		snapshot, snapshotErr := handle.Snapshot(t.Context())
		var messages []string
		if snapshot != nil {
			for _, item := range snapshot.ItemsSnapshot() {
				if item.Message != nil {
					messages = append(messages, fmt.Sprintf("role=%s content=%q calls=%#v", item.Message.Message.Role, item.Message.Message.Content, item.Message.Message.ToolCalls))
				}
			}
		}
		t.Fatalf("expected at least two provider calls, calls=%d offered=%v messages=%v snapshotErr=%v", model.calls, model.offered, messages, snapshotErr)
	}
	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(persisted), "updated host edit")
	require.GreaterOrEqual(t, len(model.offered), 2)
	require.Contains(t, model.offered[0], teamedit.ToolName)
	require.Contains(t, model.offered[0], teamedit.PersistToolName)
	require.Contains(t, model.offered[1], teamedit.PersistToolName)
}
