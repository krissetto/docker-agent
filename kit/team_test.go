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
	require.Len(t, cfg.Agents, 7)
	for _, expected := range []struct {
		name      string
		provider  string
		modelName string
		model     string
		effort    string
		subagents []string
		toolsets  []string
		readFile  bool
	}{
		{"root", "openai", "gpt-6.1-sol", "gpt-6.1-sol", "medium", []string{"director", "engineer", "designer", "reviewer"}, []string{"filesystem", "shell", "todo"}, false},
		{"director", "openai", "gpt-6.1-sol", "gpt-6.1-sol-high", "high", []string{"greppy", "planner", "engineer", "designer", "reviewer"}, []string{"filesystem"}, true},
		{"greppy", "openai", "gpt-6.1-sol", "gpt-6.1-sol-low", "low", nil, []string{"filesystem", "shell", "todo"}, false},
		{"planner", "openai", "gpt-6.1-sol", "gpt-6.1-sol-high", "high", nil, []string{"filesystem", "shell", "todo"}, true},
		{"engineer", "openai", "gpt-6.1-sol", "gpt-6.1-sol", "medium", nil, []string{"filesystem", "shell", "todo"}, false},
		{"designer", "anthropic", "claude-opus-5-5", "claude-opus-5-5", "adaptive/high", nil, []string{"filesystem", "shell", "todo"}, false},
		{"reviewer", "openai", "gpt-6.1-sol", "gpt-6.1-sol-high", "high", nil, []string{"filesystem", "shell"}, false},
	} {
		t.Run(expected.name, func(t *testing.T) {
			a, ok := cfg.Agents.Lookup(expected.name)
			require.True(t, ok)
			assert.Empty(t, a.SubAgents)
			assert.Equal(t, expected.model, a.Model)
			model, ok := cfg.Models[a.Model]
			require.True(t, ok)
			assert.Equal(t, expected.provider, model.Provider)
			assert.Equal(t, expected.modelName, model.Model)
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
			assert.NotContains(t, strings.ToLower(a.Instruction), "implementer")
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
	descriptor, err := os.ReadFile("async-agent.yaml")
	require.NoError(t, err)
	var kit struct {
		SchemaVersion string                    `yaml:"schemaVersion"`
		Kind          string                    `yaml:"kind"`
		Description   string                    `yaml:"description"`
		SourceURL     string                    `yaml:"sourceUrl"`
		Licenses      []string                  `yaml:"licenses"`
		Version       string                    `yaml:"version"`
		Provides      []string                  `yaml:"provides"`
		Dockerfile    string                    `yaml:"dockerfile"`
		Args          map[string]map[string]any `yaml:"args"`
		Capabilities  []struct {
			Type     string         `yaml:"type"`
			Optional bool           `yaml:"optional"`
			Config   map[string]any `yaml:"config"`
		} `yaml:"capabilities"`
	}
	require.NoError(t, yaml.Unmarshal(descriptor, &kit))
	var fields map[string]any
	require.NoError(t, yaml.Unmarshal(descriptor, &fields))
	assert.NotContains(t, fields, "name", "v3 identity comes from the reference, not a descriptor name")
	assert.Equal(t, "3", kit.SchemaVersion)
	assert.Equal(t, "workload", kit.Kind)
	assert.NotEmpty(t, kit.Description)
	assert.Equal(t, "https://github.com/docker/docker-agent", kit.SourceURL)
	assert.Equal(t, []string{"Apache-2.0"}, kit.Licenses)
	assert.Equal(t, "${{ kit.args.kitVersion }}", kit.Version)
	assert.Equal(t, []string{"async-agent-kit@${{ kit.args.kitVersion }}"}, kit.Provides)
	assert.Equal(t, "./async-agent.dockerfile", kit.Dockerfile)
	require.Len(t, kit.Args, 2)
	assert.Equal(t, "0.1.0", kit.Args["kitVersion"]["default"])
	assert.Equal(t, `^[0-9]+\.[0-9]+\.[0-9]+$`, kit.Args["kitVersion"]["pattern"])
	assert.Equal(t, "ASYNC_AGENT_KIT_VERSION", kit.Args["kitVersion"]["buildArg"])
	assert.Equal(t, "off", kit.Args["diagnostics"]["default"])
	assert.Equal(t, []any{"off", "on"}, kit.Args["diagnostics"]["enum"])
	assert.Equal(t, "ASYNC_AGENT_KIT_DIAGNOSTICS", kit.Args["diagnostics"]["env"])
	assert.True(t, strings.HasPrefix(string(descriptor), "# syntax=docker/sandbox-kit:3@sha256:11bb68806aef6a68d45c10c0c3b3dc86c2f794a296b5a933661d9dadf760b753\n"))
	byType := make(map[string][]int)
	for i, capability := range kit.Capabilities {
		byType[capability.Type] = append(byType[capability.Type], i)
	}
	for _, kind := range []string{"network-policy", "sbx", "agent-sessions", "lifecycle"} {
		require.Len(t, byType["com.docker.sandbox/"+kind+"@1"], 1)
	}
	assert.Nil(t, kit.Capabilities[byType["com.docker.sandbox/sbx@1"][0]].Config)
	assert.Equal(t, map[string]any{
		"prompt":   []any{"--exec", "--", "{{.Prompt}}"},
		"resume":   []any{"--session", "{{.SessionID}}"},
		"continue": []any{"--session=-1"},
	}, kit.Capabilities[byType["com.docker.sandbox/agent-sessions@1"][0]].Config)
	lifecycle := kit.Capabilities[byType["com.docker.sandbox/lifecycle@1"][0]]
	assert.True(t, lifecycle.Optional)
	require.Len(t, lifecycle.Config, 2)
	recipe, err := os.ReadFile("async-agent.dockerfile")
	require.NoError(t, err)
	assert.Contains(t, string(recipe), "xx-go build")
	assert.Contains(t, string(recipe), `ARG ASYNC_AGENT_KIT_VERSION="0.1.0"`)
	assert.Contains(t, string(recipe), `LABEL com.docker.async-agent.kit.version=$ASYNC_AGENT_KIT_VERSION`)
	assert.Contains(t, string(recipe), "xx-verify --static /docker-agent")
	assert.Contains(t, string(recipe), `ENTRYPOINT ["/opt/async-agent/launch.sh"]`)
	assert.Contains(t, string(recipe), `COPY kit/hackerspace.yaml /opt/async-agent/hackerspace.yaml`)
	assert.NotContains(t, string(recipe), "COPY . ")
	ignore, err := os.ReadFile("async-agent.dockerfile.dockerignore")
	require.NoError(t, err)
	assert.Contains(t, string(ignore), "!kit/hackerspace.yaml\n")
}

func TestKitPublicationIdentity(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	docs := string(readme)
	for _, namespace := range []string{"christopherpetito053", "christopherpetito234"} {
		assert.Contains(t, docs, namespace+"/kagent")
		assert.NotContains(t, docs, "REPO='"+namespace+"/docker-agent'")
	}
	assert.NotContains(t, docs, "christopherpetito053/async-agent")
	assert.NotContains(t, docs, "christopherpetito234/async-agent")
	assert.Contains(t, docs, "built-in `docker-agent`")
	assert.Contains(t, docs, "task kit -- namespace/kagent:trial")
	assert.Contains(t, docs, "KIT_REF='christopherpetito053/kagent@sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442'")
	assert.NotContains(t, docs, "REPLACE_WITH_VERIFIED_PUBLISHED_DIGEST")
	assert.Contains(t, docs, "christopherpetito053/docker-agent@sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442")
	assert.NotContains(t, docs, "KIT_REF='christopherpetito053/docker-agent@sha256:1c2ec35")
	assert.Contains(t, docs, "docker buildx build . -f kit/async-agent.yaml")
	assert.Contains(t, docs, "does not support this nested source layout")
	assert.NotContains(t, docs, "sbx run --name async-agent-source-trial-1")
	assert.NotContains(t, docs, "sbx kit inspect async-agent.yaml")
}

func TestKitDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX lifecycle hooks")
	}
	descriptor, err := os.ReadFile("async-agent.yaml")
	require.NoError(t, err)
	type hook struct {
		Command []string `yaml:"command"`
		User    string   `yaml:"user"`
		Env     []string `yaml:"env"`
	}
	var kit struct {
		Capabilities []struct {
			Type   string `yaml:"type"`
			Config struct {
				Install []hook `yaml:"install"`
				Startup []hook `yaml:"startup"`
			} `yaml:"config"`
		} `yaml:"capabilities"`
	}
	require.NoError(t, yaml.Unmarshal(descriptor, &kit))
	var install, startup []hook
	for _, capability := range kit.Capabilities {
		if capability.Type == "com.docker.sandbox/lifecycle@1" {
			install, startup = capability.Config.Install, capability.Config.Startup
		}
	}
	require.Len(t, install, 1)
	require.Len(t, startup, 1)
	home := t.TempDir()
	directory := filepath.Join(home, ".local/state/async-agent-kit/probe")
	run := func(h hook, enabled string) {
		t.Helper()
		assert.Equal(t, "agent", h.User)
		require.Equal(t, []string{"ASYNC_AGENT_KIT_DIAGNOSTICS"}, h.Env)
		require.Len(t, h.Command, 4)
		assert.Equal(t, []string{"/bin/sh", "-eu", "-c"}, h.Command[:3])
		// Run only on this test's owned home, with the declared environment.
		command := strings.ReplaceAll(h.Command[3], "/home/agent", home)
		cmd := exec.CommandContext(t.Context(), h.Command[0], h.Command[1], h.Command[2], command)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "ASYNC_AGENT_KIT_DIAGNOSTICS=" + enabled}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Empty(t, out)
	}
	run(install[0], "off")
	run(startup[0], "off")
	assert.NoDirExists(t, directory)
	run(install[0], "on")
	for range 2 {
		run(startup[0], "on")
		ready, err := os.ReadFile(filepath.Join(directory, "ready"))
		require.NoError(t, err)
		assert.Equal(t, "ready\n", string(ready))
	}
	log, err := os.ReadFile(filepath.Join(directory, "install.log"))
	require.NoError(t, err)
	assert.Equal(t, "installed\n", string(log))
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "install.log", entries[0].Name())
	assert.Equal(t, "ready", entries[1].Name())
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
	workspace, err = filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
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
