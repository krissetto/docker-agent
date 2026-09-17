package asyncsubagents_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestTeam(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.NewFileSource("hackerspace.yaml"))
	require.NoError(t, err)
	require.Len(t, cfg.Agents, 6)
	for _, expected := range []struct {
		name      string
		model     string
		effort    string
		subagents []string
		toolsets  []string
		readFile  bool
	}{
		{"root", "gpt-6-astra", "medium", []string{"director", "implementer", "reviewer"}, []string{"filesystem", "shell", "todo"}, false},
		{"director", "gpt-6-astra-high", "high", []string{"greppy", "planner", "implementer"}, []string{"filesystem"}, true},
		{"greppy", "gpt-6-astra-low", "low", nil, []string{"filesystem", "shell", "todo"}, false},
		{"planner", "gpt-6-astra-high", "high", nil, []string{"filesystem", "shell", "todo"}, true},
		{"implementer", "gpt-6-astra", "medium", nil, []string{"filesystem", "shell", "todo"}, false},
		{"reviewer", "gpt-6-astra-high", "high", nil, []string{"filesystem", "shell"}, false},
	} {
		t.Run(expected.name, func(t *testing.T) {
			a, ok := cfg.Agents.Lookup(expected.name)
			require.True(t, ok)
			assert.Empty(t, a.SubAgents)
			assert.Equal(t, expected.model, a.Model)
			model, ok := cfg.Models[a.Model]
			require.True(t, ok)
			assert.Equal(t, "openai", model.Provider)
			assert.Equal(t, "gpt-6-astra", model.Model)
			require.NotNil(t, model.ThinkingBudget)
			assert.Equal(t, expected.effort, model.ThinkingBudget.Effort)
			assert.ElementsMatch(t, expected.subagents, a.Subagents.AgentNames())
			for _, child := range a.Subagents {
				_, ok := cfg.Agents.Lookup(child.Agent)
				assert.True(t, ok)
				assert.NotEmpty(t, child.Description)
			}
			require.Len(t, a.Toolsets, len(expected.toolsets))
			for i, toolset := range a.Toolsets {
				assert.Equal(t, expected.toolsets[i], toolset.Type)
				if i == 0 && expected.readFile {
					assert.Equal(t, []string{"read_file"}, toolset.Tools)
				} else {
					assert.Empty(t, toolset.Tools)
				}
			}
			assert.NotEmpty(t, a.Instruction)
			assert.NotContains(t, a.Instruction, "# Async subagents")
		})
	}
	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	assert.Equal(t, "yo, shelly here.", root.WelcomeMessage)
	allowed := []subagent.AllowedSubagent{{Agent: root.Subagents[0].Agent}}
	_, ok = subagent.FindAllowed(allowed, "director")
	assert.True(t, ok)
	_, ok = subagent.FindAllowed(allowed, "undeclared")
	assert.False(t, ok)
	require.NoError(t, config.ApplyModelOverrides(cfg, []string{"openai/another-model"}))
	for _, a := range cfg.Agents {
		assert.Equal(t, "openai/another-model", a.Model)
	}
}

func TestNativeKit(t *testing.T) {
	descriptor, err := os.ReadFile("../../../async-agent.yaml")
	require.NoError(t, err)
	var kit struct {
		SchemaVersion string `yaml:"schemaVersion"`
		Kind          string `yaml:"kind"`
		Capabilities  []struct {
			Type   string         `yaml:"type"`
			Config map[string]any `yaml:"config"`
		} `yaml:"capabilities"`
	}
	require.NoError(t, yaml.Unmarshal(descriptor, &kit))
	assert.Equal(t, "3", kit.SchemaVersion)
	assert.Equal(t, "workload", kit.Kind)
	require.Len(t, kit.Capabilities, 2)
	assert.Equal(t, "com.docker.runtime/network-policy@1", kit.Capabilities[0].Type)
	assert.Equal(t, "com.docker.runtime/credential@1", kit.Capabilities[1].Type)
	assert.Equal(t, "openai", kit.Capabilities[1].Config["service"])
	recipe, err := os.ReadFile("../../../async-agent.dockerfile")
	require.NoError(t, err)
	assert.Contains(t, string(recipe), "xx-go build")
	assert.Contains(t, string(recipe), "xx-verify --static /docker-agent")
	assert.Contains(t, string(recipe), `ENTRYPOINT ["/opt/async-agent/launch.sh"]`)
	assert.Contains(t, string(recipe), `COPY examples/sbx/async-subagents/hackerspace.yaml /opt/async-agent/hackerspace.yaml`)
	assert.NotContains(t, string(recipe), "COPY . ")
	ignore, err := os.ReadFile("../../../async-agent.dockerfile.dockerignore")
	require.NoError(t, err)
	assert.Contains(t, string(ignore), "!examples/sbx/async-subagents/hackerspace.yaml\n")
}

func TestLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runtime launcher")
	}
	script, err := os.ReadFile("launch.sh")
	require.NoError(t, err)
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	// Substitute only the installation prefix; the runtime arguments remain real.
	script = []byte(strings.ReplaceAll(string(script), "/opt/async-agent", home))
	path := filepath.Join(home, "launch.sh")
	require.NoError(t, os.WriteFile(path, script, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "docker-agent"), []byte("#!/bin/sh\nprintf '%s\\n' \"$DOCKER_AGENT_AUTO_UPDATE\" \"$@\"\n"), 0o700))
	for _, editable := range []bool{false, true} {
		if editable {
			require.NoError(t, os.WriteFile(filepath.Join(workspace, "hackerspace.yaml"), []byte("agents: {}"), 0o600))
		}
		cmd := exec.CommandContext(t.Context(), "sh", path, "--model", "openai/example", "--dry-run")
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "DOCKER_AGENT_AUTO_UPDATE=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		configDir := home
		if editable {
			configDir = workspace
		}
		expected := []string{
			"0", "run", filepath.Join(configDir, "hackerspace.yaml"),
			"--working-dir", workspace,
			"--model", "openai/example", "--dry-run",
		}
		assert.Equal(t, expected, strings.Split(strings.TrimSpace(string(out)), "\n"))
		assert.NoDirExists(t, filepath.Join(workspace, ".docker-agent-try"))
	}
}
