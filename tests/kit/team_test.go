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

func TestBundledTeamSubagentDescriptionParses(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.NewFileSource(kitPath(t, "hackerspace.yaml")))
	require.NoError(t, err)
	engineer, ok := cfg.Agents.Lookup("engineer")
	require.True(t, ok)
	for _, child := range engineer.Subagents {
		if child.Agent == "greppy" {
			assert.Equal(t, "Spawn one or more in parallel to explore codebases", child.Description)
			return
		}
	}
	t.Fatal("engineer is missing its greppy subagent declaration")
}

func TestTeam(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.NewFileSource(kitPath(t, "hackerspace.yaml")))
	require.NoError(t, err)
	require.Len(t, cfg.Agents, 11)
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
		{"root", "openai", "gpt-6-astra", "gpt-6-astra", "medium", []string{"orchestrator"}, []string{"shell", "todo"}, false},
		{"orchestrator", "openai", "gpt-6.1-sol", "gpt-6.1-sol-xhigh", "xhigh", []string{"dev-team", "engineer", "designer", "review-team"}, []string{"todo"}, false},
		{"dev-team", "anthropic", "claude-opus-5-5", "opus-5.5-high", "adaptive/high", []string{"greppy", "planner", "engineer", "designer", "technical-reviewer"}, []string{"shell"}, false},
		{"greppy", "anthropic", "claude-haiku-5-5", "haiku-5.5", "adaptive/medium", nil, []string{"shell", "todo"}, false},
		{"planner", "openai", "gpt-6.1-sol", "gpt-6.1-sol", "medium", nil, []string{"shell", "todo"}, false},
		{"engineer", "anthropic", "claude-haiku-5-5", "haiku-5.5-high", "adaptive/high", []string{"designer", "greppy"}, []string{"shell", "todo"}, false},
		{"designer", "anthropic", "claude-opus-5-5", "opus-5.5-high", "adaptive/high", nil, []string{"shell", "filesystem", "todo"}, true},
		{"review-team", "openai", "gpt-6-astra", "gpt-6-astra-xhigh", "xhigh", []string{"technical-reviewer", "security-reviewer", "edge-case-reviewer", "designer"}, []string{"shell", "todo"}, false},
		{"technical-reviewer", "anthropic", "claude-haiku-5-5", "haiku-5.5-xhigh", "adaptive/xhigh", nil, []string{"shell", "todo"}, false},
		{"security-reviewer", "anthropic", "claude-haiku-5-5", "haiku-5.5-xhigh", "adaptive/xhigh", nil, []string{"shell", "todo"}, false},
		{"edge-case-reviewer", "anthropic", "claude-haiku-5-5", "haiku-5.5-high", "adaptive/high", nil, []string{"shell", "todo"}, false},
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
	_, ok = subagent.FindAllowed(allowed, "orchestrator")
	assert.True(t, ok)
	_, ok = subagent.FindAllowed(allowed, "dev-team")
	assert.False(t, ok, "root reaches dev-team only through orchestrator")
	_, ok = subagent.FindAllowed(allowed, "undeclared")
	assert.False(t, ok)
	require.NoError(t, config.ApplyModelOverrides(cfg, []string{"openai/another-model"}))
	for _, a := range cfg.Agents {
		assert.Equal(t, "openai/another-model", a.Model)
	}
}

func TestTeamReviewRouting(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.NewFileSource(kitPath(t, "hackerspace.yaml")))
	require.NoError(t, err)
	for _, expected := range []struct {
		name      string
		reviewers []string
	}{
		{"root", nil},
		{"orchestrator", []string{"review-team"}},
		{"dev-team", []string{"technical-reviewer"}},
		{"review-team", []string{"technical-reviewer", "security-reviewer", "edge-case-reviewer", "designer"}},
		{"technical-reviewer", nil},
		{"security-reviewer", nil},
		{"edge-case-reviewer", nil},
	} {
		t.Run(expected.name, func(t *testing.T) {
			a, ok := cfg.Agents.Lookup(expected.name)
			require.True(t, ok)
			var reviewers []string
			for _, name := range a.Subagents.AgentNames() {
				if name == "review-team" || strings.HasSuffix(name, "-reviewer") || (expected.name == "review-team" && name == "designer") {
					reviewers = append(reviewers, name)
				}
			}
			assert.ElementsMatch(t, expected.reviewers, reviewers)
		})
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
	// Keep this prompt budget tied to the pre-shortening working-tree snapshot.
	// orchestrator postdates that snapshot, so it is checked for guidance only.
	baselineWords := map[string]int{
		"root": 649, "dev-team": 369, "greppy": 175, "planner": 231,
		"engineer": 430, "designer": 398, "review-team": 319,
		"technical-reviewer": 200, "security-reviewer": 177, "edge-case-reviewer": 180,
	}
	guidance := map[string][]string{
		"root":               {"warm makerspace counter", "default: 1–3 short bullets/sentences", "no routine progress, recap, or roleplay", "always delegate tasks of meaningful size to `orchestrator`", "reviews happen only on explicit request", "fresh children lack parent chat", "essential facts/constraints", "safety first"},
		"orchestrator":       {"own delegation and coordination, never implementation", "dev team owns substantial development", "engineer handles bounded implementation/debugging/general tasks", "review team handles reviews only when the user explicitly requests one", "one owner per task", "fresh children lack parent chat", "default: 1–3 short friendly bullets/sentences", "distinguish verified results from assumptions"},
		"dev-team":           {"lead the crew", "direct, don't implement", "engineer implements", "technical reviewer is optional on larger tasks, never routine", "user-requested reviews go to `review-team`", "fresh children lack parent chat", "one owner; parallelize, avoid polling/duplicates", "default: 1–3 short friendly bullets/sentences"},
		"greppy":             {"greppy scouts the workshop shelves", "trace assigned questions", "don't edit/write tests", "default: 1–3 friendly bullets", "finding, evidence/paths, uncertainty", "no routine progress or recap"},
		"planner":            {"planner maps the bench", "don't edit", "own routes, trade-offs, checks", "engineer implements", "designer offers targeted ux, never required", "default: 1–3 short friendly bullets/sentences"},
		"engineer":           {"steady bench maker", "default for implementation, debugging, general work", "own integration", "work autonomously", "safety/correctness first", "only recruit declared designer for targeted ux/bounded frontend", "fresh children lack parent chat", "smallest complete change", "run focused checks", "default: 1–3 short friendly bullets/sentences", "no routine progress or recap"},
		"designer":           {"engineer is default", "designer owns assigned ux/frontend work", "advice means no edits", "requested reviews are read-only", "accessible layouts/interactions", "don't recruit, widen, or redesign neighbors", "default: 1–3 brief friendly bullets/sentences", "no routine progress or recap"},
		"review-team":        {"review lead keeps the makerspace side bench", "review only on explicit request, never routinely", "development lead owns fixes", "choose a small team", "designer only for requested ux review, read-only", "fresh children lack parent chat", "default: 1–3 short friendly bullets/sentences", "no routine progress or recap"},
		"technical-reviewer": {"technical reviewer checks the measuring bench", "review assigned user requests", "never routine/dev", "read-only", "default: 1–3 short friendly bullets", "no routine progress/recap"},
		"security-reviewer":  {"key keeper", "review assigned scope; no routine", "never expose, repeat, log, or report secrets", "read-only", "default: 1–3 friendly bullets", "no progress/recap"},
		"edge-case-reviewer": {"edge case reviewer tests awkward corners", "review assigned scope; no routine/dev", "cancellation/retry", "recovery", "read-only", "default: 1–3 friendly bullets", "no progress/recap"},
	}
	require.Len(t, cfg.Agents, len(guidance))
	for name, semantics := range guidance {
		a, ok := cfg.Agents.Lookup(name)
		require.True(t, ok, "missing agent %s", name)
		if baseline, ok := baselineWords[name]; ok {
			assert.LessOrEqual(t, len(strings.Fields(a.Instruction)), baseline/4, "%s prompt must be at least 75%% shorter", name)
		}
		instruction := strings.Join(strings.Fields(strings.ToLower(a.Instruction)), " ")
		for _, semantic := range semantics {
			assert.Contains(t, instruction, semantic, "%s prompt", name)
		}
	}
	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.NotNil(t, root.CompactionThreshold)
	assert.Equal(t, 0.25, *root.CompactionThreshold)
	assert.Equal(t, "haiku-5.5", root.CompactionModel)
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
	require.Len(t, kit.Args, 6)
	assert.Equal(t, "0.1.0", kit.Args["kitVersion"]["default"])
	assert.Equal(t, `^[0-9]+\.[0-9]+\.[0-9]+$`, kit.Args["kitVersion"]["pattern"])
	assert.Equal(t, "ASYNC_AGENT_KIT_VERSION", kit.Args["kitVersion"]["buildArg"])
	assert.Equal(t, "auto", kit.Args["team"]["default"])
	assert.Equal(t, "ASYNC_AGENT_KIT_TEAM", kit.Args["team"]["env"])
	assert.Equal(t, "", kit.Args["model"]["default"])
	assert.Equal(t, "ASYNC_AGENT_KIT_MODEL", kit.Args["model"]["env"])
	assert.Equal(t, "/home/agent/.cagent/managed-api", kit.Args["stateDir"]["default"])
	assert.Equal(t, "ASYNC_AGENT_KIT_STATE_DIR", kit.Args["stateDir"]["env"])
	assert.Equal(t, "off", kit.Args["teamEdit"]["default"])
	assert.Equal(t, []any{"off", "restricted", "full"}, kit.Args["teamEdit"]["enum"])
	assert.Equal(t, "ASYNC_AGENT_KIT_TEAM_EDIT", kit.Args["teamEdit"]["env"])
	assert.Equal(t, "off", kit.Args["diagnostics"]["default"])
	assert.Equal(t, []any{"off", "on"}, kit.Args["diagnostics"]["enum"])
	assert.Equal(t, "ASYNC_AGENT_KIT_DIAGNOSTICS", kit.Args["diagnostics"]["env"])
	assert.True(t, strings.HasPrefix(string(descriptor), "# syntax=docker/sandbox-kit:210ccc3@sha256:8944c950eaaef7e1f33bd4c672e9607c8deb33eedd49d62d8ac58b2d4045de99\n"))
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
	assert.NotContains(t, string(descriptor), "${{ kit.env.WORKSPACE_DIR }}", "SBX supplies WORKSPACE_DIR at runtime, after descriptor resolution")
	assert.Equal(t, map[string]any{
		"prompt":   []any{"--exec", "--", "{{.Prompt}}"},
		"resume":   []any{"--exec", "--session", "{{.SessionID}}"},
		"continue": []any{"--exec", "--session=-1"},
		"list":     []any{"/opt/async-agent/docker-agent", "sessions", "list", "--managed-api", "--managed-api-attach", "--managed-api-state-dir", "${{ kit.env.ASYNC_AGENT_KIT_STATE_DIR }}", "--quiet"},
	}, kit.Capabilities[byType["com.docker.sandbox/agent-sessions@1"][0]].Config)
	interactive := kit.Capabilities[byType["com.docker.sandbox/agent-interactive-sessions@1"][0]]
	assert.Equal(t, map[string]any{
		"prompt":     []any{"--", "{{.Prompt}}"},
		"resume":     []any{"--session", "{{.SessionID}}"},
		"continue":   []any{"--session=-1"},
		"newSession": []any{},
		"list":       []any{"/opt/async-agent/docker-agent", "sessions", "list", "--managed-api", "--managed-api-attach", "--managed-api-state-dir", "${{ kit.env.ASYNC_AGENT_KIT_STATE_DIR }}", "--quiet"},
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
	assert.Equal(t, []string{"/opt/async-agent/start.sh", "${{ kit.env.ASYNC_AGENT_KIT_TEAM }}", "${{ kit.env.ASYNC_AGENT_KIT_STATE_DIR }}", "${{ kit.env.ASYNC_AGENT_KIT_MODEL }}", "${{ kit.env.ASYNC_AGENT_KIT_TEAM_EDIT }}"}, startup[1].Command)
	assert.Contains(t, startup[1].Env, "OPENAI_API_KEY")
	assert.Contains(t, startup[1].Env, "ANTHROPIC_API_KEY")
	assert.Contains(t, startup[1].Env, "GOOGLE_API_KEY")
	assert.Contains(t, startup[1].Env, "GH_TOKEN")
	assert.Contains(t, startup[1].Env, "ASYNC_AGENT_KIT_TEAM_EDIT")
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
