package root

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui"
)

// TestSpawnerOwnsRuntimeForAnotherWorkingDir pins the tab/working-directory
// contract: a tab opened in a directory other than the running runtime's gets
// a runtime of its own whose toolsets operate in that directory, while a tab
// in the same directory borrows the shared session registry.
func TestSpawnerOwnsRuntimeForAnotherWorkingDir(t *testing.T) {
	baseDir := t.TempDir()
	otherDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(otherDir, "marker.txt"), []byte("here"), 0o644))

	agentFile := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: You are a test agent.
    toolsets:
      - type: filesystem
`), 0o644))
	agentSource, err := sources.Resolve(agentFile, nil)
	require.NoError(t, err)

	flags := &runExecFlags{}
	flags.runConfig = config.RuntimeConfig{
		WorkingDir:          baseDir,
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
	}
	loadResult, err := flags.loadAgentFrom(t.Context(), flags.loadTeamRequest(agentSource))
	require.NoError(t, err)
	t.Cleanup(func() { stopToolSets(context.WithoutCancel(t.Context()), loadResult.Team) })

	services, sessions, _, cleanup, err := (&localBackend{flags: flags, agentSource: agentSource}).CreateSession(t.Context(), loadResult, flags.createSessionRequest(baseDir))
	require.NoError(t, err)
	t.Cleanup(cleanup)

	spawner := flags.createSessionSpawner(agentSource, services, sessions)
	require.NotNil(t, spawner)

	borrowed, err := spawner(t.Context(), baseDir)
	require.NoError(t, err)
	assert.Equal(t, tui.RuntimeBorrowed, borrowed.Ownership, "same directory keeps the shared runtime")
	assert.Same(t, sessions, borrowed.App.SessionRuntime())

	owned, err := spawner(t.Context(), otherDir)
	require.NoError(t, err)
	t.Cleanup(owned.Cleanup)
	assert.Equal(t, tui.RuntimeOwned, owned.Ownership, "another directory gets its own runtime")
	require.NotNil(t, owned.Cleanup)
	assert.NotSame(t, sessions, owned.App.SessionRuntime())
	assert.Equal(t, otherDir, owned.Session.WorkingDir)

	// The owned runtime's filesystem tools are rooted in the new directory.
	toolDefs, err := owned.App.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	var listDir *tools.Tool
	for i := range toolDefs {
		if toolDefs[i].Name == "list_directory" {
			listDir = &toolDefs[i]
		}
	}
	require.NotNil(t, listDir, "the spawned runtime must expose the filesystem toolset")
	result, err := listDir.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "list_directory", Arguments: `{"path":"."}`}}, nil)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "marker.txt", "tools must operate in the tab's working directory")
}
