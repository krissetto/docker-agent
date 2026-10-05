package host

import (
	"context"
	"errors"
	"path/filepath"
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
		owner, err := NewSessionRuntime(t.Context(), manifest.FrozenSource(), &config.RuntimeConfig{ModelsDevStoreOverride: catalog, EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=fake-provider-only"})}, session.NewInMemorySessionStore(), RuntimeOptions{WorkingDir: startup.Workspace, Snapshots: startup.Snapshots, LoaderOptions: []teamloader.Opt{teamloader.WithProviderRegistry(registry)}})
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
