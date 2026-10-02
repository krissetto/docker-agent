package asyncsubagents_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestKitContextAndSkillsDiscovery(t *testing.T) {
	teamYAML, err := os.ReadFile("hackerspace.yaml")
	require.NoError(t, err)
	cfg, err := config.Load(t.Context(), config.NewFileSource("hackerspace.yaml"))
	require.NoError(t, err)
	profile, err := os.ReadFile("async-agent-context.md")
	require.NoError(t, err)
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace", "project")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv(skills.KitDirEnv, "")
	t.Chdir(workspace)
	// SBX writes the profile beside the workspace, not over project AGENTS.md.
	profilePath := filepath.Join(filepath.Dir(workspace), "ASYNC_AGENT_KIT.md")
	require.NoError(t, os.WriteFile(profilePath, profile, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("Project-specific guidance."), 0o600))
	skillPath := filepath.Join(home, ".agents", "skills", "shared", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o700))
	require.NoError(t, os.WriteFile(skillPath, []byte("---\nname: shared\ndescription: Shared fixture guidance\n---\nStay within existing tool scopes.\n"), 0o600))
	registry := hooks.NewRegistry()
	require.NoError(t, builtins.Register(registry))
	loadPromptFiles, ok := registry.LookupBuiltin(builtins.AddPromptFiles)
	require.True(t, ok)
	for _, agent := range cfg.Agents {
		out, err := loadPromptFiles(t.Context(), &hooks.Input{Cwd: workspace}, agent.AddPromptFiles)
		require.NoError(t, err)
		require.Len(t, out.HookSpecificOutput.InstructionContext, 2)
		assert.Contains(t, out.HookSpecificOutput.InstructionContext[0].Content, "Project-specific guidance.")
		assert.Contains(t, out.HookSpecificOutput.InstructionContext[1].Content, string(profile))
		loaded := skills.Load(t.Context(), agent.Skills.Sources)
		if !agent.Skills.Enabled() {
			assert.Empty(t, loaded)
			continue
		}
		require.Len(t, loaded, 1)
		assert.Equal(t, "shared", loaded[0].Name)
		assert.Equal(t, skillPath, loaded[0].FilePath)
	}
	// Exercise the real team loader/tool filters with an inert provider factory:
	// no credentials, model requests, or real-home discovery are involved.
	store, err := modelsdev.NewStore(modelsdev.WithCache(filepath.Join(home, "catalog.json")), modelsdev.WithKnownProvider(func(string) bool { return false }))
	require.NoError(t, err)
	inert := func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
		return nil, nil
	}
	team, err := teamloader.Load(t.Context(), config.NewBytesSource("hackerspace.yaml", teamYAML), &config.RuntimeConfig{
		Config:                 config.Config{WorkingDir: workspace},
		EnvProviderForTests:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fixture", "ANTHROPIC_API_KEY": "fixture"}),
		ModelsDevStoreOverride: store,
	}, teamloader.WithProviderRegistry(provider.NewRegistry(map[string]provider.Factory{"openai": inert, "anthropic": inert})),
		teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{
			"filesystem": filesystem.Creator, "shell": shell.Creator, "todo": teamloader.CreatorFromToolset(todo.CreateToolSet),
		})))
	require.NoError(t, err)
	defer team.StopToolSets(t.Context())
	for _, name := range []string{"root", "director", "greppy", "planner", "engineer", "designer", "reviewer"} {
		a, err := team.Agent(name)
		require.NoError(t, err)
		actual, err := a.Tools(t.Context())
		require.NoError(t, err)
		var names []string
		for _, tool := range actual {
			names = append(names, tool.Name)
		}
		if name == "root" || name == "engineer" || name == "designer" {
			assert.Contains(t, names, "read_skill")
		} else {
			assert.NotContains(t, names, "read_skill")
			assert.NotContains(t, names, "read_skill_file")
			assert.NotContains(t, names, "run_skill")
		}
		if name == "director" {
			assert.ElementsMatch(t, []string{"read_file", "spawn_subagent", "send_message", "read_subagent", "stop_subagent"}, names)
		}
	}
	// A custom team replaces, rather than inherits, the bundled loaders.
	customPath := filepath.Join(workspace, "custom.yaml")
	require.NoError(t, os.WriteFile(customPath, []byte("agents:\n  root:\n    model: openai/example\n"), 0o600))
	custom, err := config.Load(t.Context(), config.NewFileSource(customPath))
	require.NoError(t, err)
	root, ok := custom.Agents.Lookup("root")
	require.True(t, ok)
	assert.Empty(t, root.AddPromptFiles)
	assert.False(t, root.Skills.Enabled())
	out, err := loadPromptFiles(t.Context(), &hooks.Input{Cwd: workspace}, root.AddPromptFiles)
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.Empty(t, skills.Load(t.Context(), root.Skills.Sources))
	// Omitting the optional host integrations leaves project guidance usable.
	require.NoError(t, os.Remove(profilePath))
	require.NoError(t, os.Remove(skillPath))
	out, err = loadPromptFiles(t.Context(), &hooks.Input{Cwd: workspace}, []string{"AGENTS.md", "ASYNC_AGENT_KIT.md"})
	require.NoError(t, err)
	require.Len(t, out.HookSpecificOutput.InstructionContext, 1)
	assert.Empty(t, skills.Load(t.Context(), []string{"local"}))
}
