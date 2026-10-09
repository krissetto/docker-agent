package kit_test

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
	cfg, err := config.Load(t.Context(), config.NewFileSource(kitPath(t, "hackerspace.yaml")))
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
		{"root", "openai", "gpt-6-astra", "gpt-6-astra", "medium", []string{"director", "engineer", "designer", "reviewer"}, []string{"shell", "todo"}, false},
		{"director", "openai", "gpt-6-astra", "gpt-6-astra-high", "high", []string{"greppy", "planner", "engineer", "designer", "reviewer"}, []string{"shell"}, false},
		{"greppy", "openai", "gpt-6.1-sol", "gpt-6.1-sol-low", "low", nil, []string{"shell", "todo"}, false},
		{"planner", "openai", "gpt-6.1-sol", "gpt-6.1-sol-high", "high", nil, []string{"shell", "todo"}, false},
		{"engineer", "openai", "gpt-6.1-sol", "gpt-6.1-sol-high", "high", []string{"designer"}, []string{"shell", "todo"}, false},
		{"designer", "anthropic", "claude-opus-5-5", "opus-5.5-high", "adaptive/high", nil, []string{"shell", "filesystem", "todo"}, true},
		{"reviewer", "openai", "gpt-6.1-sol", "gpt-6.1-sol-high", "high", nil, []string{"shell"}, false},
	} {
		t.Run(expected.name, func(t *testing.T) {
			a, ok := cfg.Agents.Lookup(expected.name)
			require.True(t, ok)
			assert.Empty(t, a.SubAgents)
			assert.Empty(t, a.AddPromptFiles)
			assert.False(t, a.Skills.Enabled())
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
				if toolset.Type == "filesystem" && expected.readFile {
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

func TestTeamModelPoolsResolve(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.NewFileSource(kitPath(t, "hackerspace.yaml")))
	require.NoError(t, err)
	for _, name := range []string{"low", "mid", "high", "xhigh"} {
		pool, ok := cfg.Models[name]
		require.True(t, ok, name)
		for alias := range strings.SplitSeq(pool.Model, ",") {
			model, ok := cfg.Models[strings.TrimSpace(alias)]
			require.True(t, ok, "%s pool references missing model %q", name, alias)
			assert.NotEmpty(t, model.Provider, alias)
			assert.NotEmpty(t, model.Model, alias)
		}
	}
}

func TestTeamGuidance(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.NewFileSource(kitPath(t, "hackerspace.yaml")))
	require.NoError(t, err)
	for _, expected := range []struct {
		name     string
		guidance []string
	}{
		{"root", []string{"use engineer more often than designer", "only when the maker explicitly asks"}},
		{"director", []string{"use engineer more often than designer", "don't micromanage", "poll for progress", "duplicate their work", "only when the user explicitly asks", "not to implement", "delegate substantial investigation, edits, tests, and integration", "use the shell only for brief orientation and spot-checking returned evidence", "don't finish the investigation before assigning it", "send corrections back to the owner", "leave delegated tasks in progress and end your turn", "reports arrive automatically"}},
		{"planner", []string{"engineer handles most implementation", "targeted ux advice", "not as a mandatory ui stage", "not a step-by-step execution script"}},
		{"engineer", []string{"default doer", "most implementation, debugging, and general tasks", "frontend changes that realize clear ux/design direction", "work autonomously", "directly recruit the declared designer subagent", "not routine ui delegation", "engineer retains implementation and integration ownership", "define file boundaries before assigning designer coding work", "other recruitment and material scope changes go through the assigning lead"}},
		{"designer", []string{"less frequent specialist than engineer", "ux advice or develops web frontend code", "react", "advice-only", "without editing code", "not own or join every ui change"}},
		{"reviewer", []string{"only when the user explicitly asks", "never as a routine gate"}},
	} {
		t.Run(expected.name, func(t *testing.T) {
			a, ok := cfg.Agents.Lookup(expected.name)
			require.True(t, ok)
			instruction := strings.Join(strings.Fields(strings.ToLower(a.Instruction)), " ")
			for _, guidance := range expected.guidance {
				assert.Contains(t, instruction, guidance)
			}
			if expected.name == "root" || expected.name == "director" {
				for _, guidance := range []string{"a task is a few plain sentences, like a message to a teammate", "what needs doing, what to bring back, and only the facts that would change what they do", "a clear request may be the whole assignment", "explicit requirements", "leave the approach"} {
					assert.Contains(t, instruction, guidance)
				}
				for _, checklist := range []string{"success criteria", "standalone goal", "don't copy your roster"} {
					assert.NotContains(t, instruction, checklist)
				}
			}
			if expected.name == "root" || expected.name == "director" || expected.name == "engineer" || expected.name == "designer" {
				for _, guidance := range []string{"simplicity", "safety or correctness", "single-line commit subjects", "descriptive branch names", "unless the user explicitly asks"} {
					assert.Contains(t, instruction, guidance)
				}
			}
		})
	}
	for _, a := range cfg.Agents {
		text := a.Description + " " + a.Instruction
		for _, child := range a.Subagents {
			text += " " + child.Description
		}
		text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
		for _, outdated := range []string{"must participate in every", "required for every task", "every frontend development or visual/ux code task", "require designer involvement"} {
			assert.NotContains(t, text, outdated)
		}
	}
}

func TestNativeKit(t *testing.T) {
	descriptor, err := os.ReadFile(kitPath(t, "async-agent.yaml"))
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
	require.Len(t, kit.Args, 5)
	assert.Equal(t, "0.1.0", kit.Args["kitVersion"]["default"])
	assert.Equal(t, `^[0-9]+\.[0-9]+\.[0-9]+$`, kit.Args["kitVersion"]["pattern"])
	assert.Equal(t, "ASYNC_AGENT_KIT_VERSION", kit.Args["kitVersion"]["buildArg"])
	assert.Equal(t, "auto", kit.Args["team"]["default"])
	assert.Equal(t, "ASYNC_AGENT_KIT_TEAM", kit.Args["team"]["env"])
	assert.Equal(t, "", kit.Args["model"]["default"])
	assert.Equal(t, "ASYNC_AGENT_KIT_MODEL", kit.Args["model"]["env"])
	assert.Equal(t, "/home/agent/.cagent/managed-api", kit.Args["stateDir"]["default"])
	assert.Equal(t, "ASYNC_AGENT_KIT_STATE_DIR", kit.Args["stateDir"]["env"])
	assert.Equal(t, "off", kit.Args["diagnostics"]["default"])
	assert.Equal(t, []any{"off", "on"}, kit.Args["diagnostics"]["enum"])
	assert.Equal(t, "ASYNC_AGENT_KIT_DIAGNOSTICS", kit.Args["diagnostics"]["env"])
	assert.True(t, strings.HasPrefix(string(descriptor), "# syntax=docker/sandbox-kit:a1d110d@sha256:7b5be06981e0a3b808b8043ad3f2d5edc5df3645ce628c9bae1bca0fc4334557\n"))
	byType := make(map[string][]int)
	for i, capability := range kit.Capabilities {
		byType[capability.Type] = append(byType[capability.Type], i)
	}
	for _, kind := range []string{"network-policy", "sbx", "agent-sessions", "agent-interactive-sessions", "lifecycle", "agent-context", "agent-skills", "git-identity", "ssh-agent", "long-running"} {
		require.Len(t, byType["com.docker.sandbox/"+kind+"@1"], 1)
	}
	require.Len(t, kit.Capabilities, 14)
	assert.NotContains(t, byType, "com.docker.sandbox/volume@1")
	for _, capability := range kit.Capabilities {
		switch capability.Type {
		case "com.docker.sandbox/credential@1":
			// Per-service requiredness and exact grants are checked in providers_test.go.
		case "com.docker.sandbox/network-policy@1", "com.docker.sandbox/sbx@1", "com.docker.sandbox/lifecycle@1", "com.docker.sandbox/long-running@1":
			assert.False(t, capability.Optional)
		default:
			assert.True(t, capability.Optional, capability.Type)
		}
	}
	for kind, expected := range map[string]map[string]any{
		"agent-context": {"filename": "ASYNC_AGENT_KIT.md"},
		"agent-skills":  {"path": "/home/agent/.agents/skills", "mode": "readonly"},
		"ssh-agent":     {"phase": "runtime", "unrestricted": false, "sign": []any{"git"}},
		"git-identity":  nil,
		"long-running":  nil,
	} {
		assert.Equal(t, expected, kit.Capabilities[byType["com.docker.sandbox/"+kind+"@1"][0]].Config, kind)
	}
	assert.Nil(t, kit.Capabilities[byType["com.docker.sandbox/sbx@1"][0]].Config)
	assert.Equal(t, map[string]any{
		"prompt":   []any{"--exec", "--", "{{.Prompt}}"},
		"resume":   []any{"--exec", "--session", "{{.SessionID}}"},
		"continue": []any{"--exec", "--session=-1"},
		"list":     []any{"/opt/async-agent/docker-agent", "sessions", "list", "--managed-api", "--managed-api-attach", "--working-dir", "/workspace", "--managed-api-state-dir", "${{ kit.env.ASYNC_AGENT_KIT_STATE_DIR }}", "--quiet"},
	}, kit.Capabilities[byType["com.docker.sandbox/agent-sessions@1"][0]].Config)
	interactive := kit.Capabilities[byType["com.docker.sandbox/agent-interactive-sessions@1"][0]]
	assert.Equal(t, map[string]any{
		"prompt":     []any{"--", "{{.Prompt}}"},
		"resume":     []any{"--session", "{{.SessionID}}"},
		"continue":   []any{"--session=-1"},
		"newSession": []any{},
		"list":       []any{"/opt/async-agent/docker-agent", "sessions", "list", "--managed-api", "--managed-api-attach", "--working-dir", "/workspace", "--managed-api-state-dir", "${{ kit.env.ASYNC_AGENT_KIT_STATE_DIR }}", "--quiet"},
	}, interactive.Config)
	assert.Equal(t, kit.Capabilities[byType["com.docker.sandbox/agent-sessions@1"][0]].Config["list"], interactive.Config["list"])
	assert.NotContains(t, interactive.Config, "sessionPicker", "the CLI has no dedicated startup picker")
	lifecycle := kit.Capabilities[byType["com.docker.sandbox/lifecycle@1"][0]]
	assert.False(t, lifecycle.Optional)
	require.Len(t, lifecycle.Config, 2)
	recipe, err := os.ReadFile(kitPath(t, "async-agent.dockerfile"))
	require.NoError(t, err)
	assert.Contains(t, string(recipe), "WORKDIR /workspace")
	assert.Contains(t, string(recipe), "COPY --chmod=0755 kit/start.sh /opt/async-agent/start.sh")
	assert.Contains(t, string(recipe), "xx-go build")
	assert.Contains(t, string(recipe), `ARG ASYNC_AGENT_KIT_VERSION="0.1.0"`)
	assert.Contains(t, string(recipe), `LABEL com.docker.async-agent.kit.version=$ASYNC_AGENT_KIT_VERSION`)
	assert.Contains(t, string(recipe), "xx-verify --static /docker-agent")
	assert.Contains(t, string(recipe), `ENTRYPOINT ["/opt/async-agent/launch.sh"]`)
	assert.Contains(t, string(recipe), `COPY kit/hackerspace.yaml /opt/async-agent/hackerspace.yaml`)
	assert.Contains(t, string(recipe), `RUN install -d -m 0700 -o agent -g agent /home/agent/.config /home/agent/.config/cagent`)
	assert.Contains(t, string(recipe), `COPY --chown=agent:agent --chmod=0600 kit/user-config.yaml /home/agent/.config/cagent/config.yaml`)
	assert.NotContains(t, string(recipe), "DOCKER_AGENT_CONFIG_DIR")
	assert.NotContains(t, string(recipe), "COPY . ")
	ignore, err := os.ReadFile(kitPath(t, "async-agent.dockerfile.dockerignore"))
	require.NoError(t, err)
	assert.Contains(t, string(ignore), "!kit/hackerspace.yaml\n")
	assert.Contains(t, string(ignore), "!kit/user-config.yaml\n")
	assert.Contains(t, string(recipe), "/home/agent/.config/cagent /home/agent/.cagent")
}

func TestKitDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX lifecycle hooks")
	}
	descriptor, err := os.ReadFile(kitPath(t, "async-agent.yaml"))
	require.NoError(t, err)
	type hook struct {
		Command    []string `yaml:"command"`
		User       string   `yaml:"user"`
		Env        []string `yaml:"env"`
		Background bool     `yaml:"background"`
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
	require.Len(t, startup, 2)
	assert.True(t, startup[1].Background)
	assert.Equal(t, "agent", startup[1].User)
	assert.Equal(t, []string{"/opt/async-agent/start.sh", "${{ kit.env.ASYNC_AGENT_KIT_TEAM }}", "${{ kit.env.ASYNC_AGENT_KIT_STATE_DIR }}", "${{ kit.env.ASYNC_AGENT_KIT_MODEL }}"}, startup[1].Command)
	assert.Contains(t, startup[1].Env, "OPENAI_API_KEY")
	assert.Contains(t, startup[1].Env, "ANTHROPIC_API_KEY")
	assert.Contains(t, startup[1].Env, "GOOGLE_API_KEY")
	assert.Contains(t, startup[1].Env, "GH_TOKEN")
	assert.NotContains(t, startup[1].Env, "CAGENT_PPROF_ADDR")
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
