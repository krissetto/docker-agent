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
		{"orchestrator", "openai", "gpt-6.1-sol", "gpt-6.1-sol-xhigh", "xhigh", []string{"dev-team", "engineer", "designer", "review-team"}, []string{"todo", "plan"}, false},
		{"dev-team", "anthropic", "claude-opus-5-5", "opus-5.5-high", "adaptive/high", []string{"greppy", "planner", "engineer", "designer", "technical-reviewer"}, []string{"shell"}, false},
		{"greppy", "anthropic", "claude-haiku-5-5", "haiku-5.5", "adaptive/medium", nil, []string{"shell", "todo"}, false},
		{"planner", "openai", "gpt-6.1-sol", "gpt-6.1-sol", "medium", nil, []string{"shell", "todo"}, false},
		{"engineer", "openai", "gpt-6.1-sol", "gpt-6.1-sol", "medium", []string{"designer", "greppy"}, []string{"shell", "todo"}, false},
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
				switch {
				case toolset.Type == "filesystem" && expected.readFile:
					assert.Equal(t, []string{"read_file"}, toolset.Tools)
				case toolset.Type == "plan":
					assert.ElementsMatch(t, []string{"list_plans", "read_plan", "write_plan"}, toolset.Tools)
					assert.NotEmpty(t, toolset.Instruction)
				default:
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
	guidance := map[string][]string{
		"root":               {"a small bot named shelly keeps the front counter", "handles these at the counter, including quick edits and checks", "hand substantial or multi-team work to `orchestrator`", "explicitly asks for a review", "trust, but verify", "say why, suggest a better option", "flattery and appeasement don't help anyone", "help carry it out within safety limits", "disagreement isn't a reason to refuse or stall"},
		"orchestrator":       {"orchestrator is the shop lead", "dev team takes substantial development and investigations", "engineer, the steady bench maker", "designer offers targeted ux advice", "review team runs independent reviews", "only when the maker explicitly asks for a review", "choose the lightest useful route", "answer or coordinate directly when that is quicker than a handoff", "make each work card concrete", "challenge weak assumptions in requests and plans with reasons", "verify factual claims before passing them on as settled", "escalate destructive or scope decisions to shelly", "disagreement isn't a reason to stall"},
		"dev-team":           {"dev team is the development crew behind the orchestrator", "greppy, the scout", "planner, the mapmaker", "engineer, the default doer", "technical reviewer, the one optional check", "never a routine gate", "use the shell yourself for brief orientation and checks", "ask greppy when the search area is genuinely unknown", "which routes them to review team", "challenge weak assumptions with reasons", "verify facts before endorsing them", "separate evidence from preference", "carry it out within safety limits"},
		"greppy":             {"greppy is the makerspace scout", "carrying a small lantern and a pocket full of labels", "read-only by default", "stop as soon as the evidence supports the decision", "verify claims against sources", "separate evidence from uncertainty", "flag shaky assumptions", "a few bullets with the strongest findings"},
		"planner":            {"planner is the makerspace mapmaker", "analysis is the default", "state the intended outcome in practical terms", "separate established context, working assumptions, and open choices", "recommend one approach", "break the work into ordered steps", "match validation checkpoints to the actual risk", "challenge weak assumptions with reasons", "verify facts before relying on them", "separate evidence from preference", "offer alternatives", "engineer can proceed with minimal rediscovery"},
		"engineer":           {"engineer is the steady bench maker", "the default for implementation, debugging, general work, and clear ui", "owns integration and works autonomously within the agreed scope", "the smallest complete change that solves the parcel", "preserve the project's architecture", "run the checks that fit it", "edit surgically", "mention it as a follow-up instead", "bring designer in only for targeted ux or bounded frontend help", "send scope changes and material surprises back to your lead", "challenge weak assumptions in the task with reasons", "once the maker chooses, carry it out within safety limits", "disagreement isn't a reason to stall", "a small edit deserves a small report"},
		"designer":           {"designer is the less frequent specialist", "owns the assigned ux or frontend work", "engineer remains the default doer", "advises by default", "requested reviews are read-only", "start from the user's goal and the existing design language", "make accessible layouts and interactions", "covering keyboard use, focus, responsive layout, and loading and error states", "preserve existing patterns", "don't redesign neighbors", "separate evidence from taste", "offer alternatives", "help build the maker's chosen direction within safety limits"},
		"review-team":        {"review team works at the bright side table", "owns the review scope and the verdict", "runs a review only when the maker explicitly asks for it", "reviews are read-only", "pick a small, complementary team from technical, security, and edge", "add designer only for a requested ux review", "challenge the scope and the evidence with reasons", "verify claims", "separate defects from uncertainty", "offer alternatives", "lead with one verdict: pass, pass with caveats, or fail"},
		"technical-reviewer": {"technical reviewer checks the measuring bench", "reviews only the scope a user explicitly requests", "stays read-only", "behavior, contracts, and integration", "send gaps and open questions to your lead", "challenge claims with reasons", "verify them against evidence", "separate defects from uncertainty"},
		"security-reviewer":  {"security reviewer keeps the keys", "review the assigned scope", "trace its inputs through authentication, authorization, injection, privacy, secrets handling, and dependency abuse", "stay read-only", "never expose, repeat, log, or report secrets", "take no external or destructive action without authorization", "challenge risky assumptions with reasons", "separate evidence from speculation"},
		"edge-case-reviewer": {"edge case reviewer tests the awkward corners", "boundaries, failures, cancellation and retry, concurrency, recovery, and state", "stay read-only", "leave security and integration to the peers", "challenge untested assumptions with reasons", "separate evidence from speculation"},
	}
	require.Len(t, cfg.Agents, len(guidance))
	for name, semantics := range guidance {
		a, ok := cfg.Agents.Lookup(name)
		require.True(t, ok, "missing agent %s", name)
		words := len(strings.Fields(a.Instruction))
		assert.GreaterOrEqual(t, words, 60, "%s prompt must keep its narrative guidance", name)
		instruction := strings.Join(strings.Fields(strings.ToLower(a.Instruction)), " ")
		for _, semantic := range semantics {
			assert.Contains(t, instruction, semantic, "%s prompt", name)
		}
		// Team-wide rules live in the harness prompts (pkg/subagent), not team YAML.
		for _, shared := range []string{"skip routine progress", "keep replies short", "result, checks", "reversible", "focused checks", "plain commit", "stop subagents once", "fresh children", "separate verified results", "explicitly asks for one"} {
			assert.NotContains(t, instruction, shared, "%s prompt", name)
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
	assert.True(t, strings.HasPrefix(string(descriptor), "# syntax=docker/sandbox-kit:d841dec@sha256:f5116e23e492215c8dcb1fcba8f43bb49f465adc7251d128e4c64a3af2eeb4f3\n"))
	byType := make(map[string][]int)
	for i, capability := range kit.Capabilities {
		byType[capability.Type] = append(byType[capability.Type], i)
	}
	for _, kind := range []string{"network-policy", "sbx", "agent-sessions", "agent-interactive-sessions", "lifecycle", "agent-skills", "git-identity", "ssh-agent", "long-running", "dockerd", "host-mount"} {
		require.Len(t, byType["com.docker.sandbox/"+kind+"@1"], 1)
	}
	require.Len(t, kit.Capabilities, 18)
	require.Len(t, byType["com.docker.sandbox/agent-context@2"], 1)
	assert.Equal(t, map[string]any{"filename": "ASYNC_AGENT_KIT.md"}, kit.Capabilities[byType["com.docker.sandbox/agent-context@2"][0]].Config)
	assert.NotContains(t, byType, "com.docker.sandbox/agent-context@1")
	require.Len(t, byType["com.docker.sandbox/auto-restart@1"], 1)
	assert.Nil(t, kit.Capabilities[byType["com.docker.sandbox/auto-restart@1"][0]].Config)
	assert.NotContains(t, byType, "com.docker.sandbox/volume@1")
	for _, capability := range kit.Capabilities {
		switch capability.Type {
		case "com.docker.sandbox/credential@1":
			// Per-service requiredness and exact grants are checked in providers_test.go.
		case "com.docker.sandbox/host-mount@1", "com.docker.sandbox/network-policy@1", "com.docker.sandbox/sbx@1", "com.docker.sandbox/lifecycle@1", "com.docker.sandbox/long-running@1":
			assert.False(t, capability.Optional)
		default:
			assert.True(t, capability.Optional, capability.Type)
		}
	}
	for kind, expected := range map[string]map[string]any{
		"dockerd":      {"autoStart": true},
		"agent-skills": {"path": "/home/agent/.agents/skills", "mode": "readonly"},
		"ssh-agent":    {"phase": "runtime", "unrestricted": false, "sign": []any{"git"}},
		"git-identity": nil,
		"long-running": nil,
	} {
		assert.Equal(t, expected, kit.Capabilities[byType["com.docker.sandbox/"+kind+"@1"][0]].Config, kind)
	}
	assert.Nil(t, kit.Capabilities[byType["com.docker.sandbox/sbx@1"][0]].Config)
	assert.NotContains(t, string(descriptor), "${{ kit.env.WORKSPACE_DIR }}", "keep workspace optional at create-time expansion")
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
	assert.Contains(t, string(recipe), `com.docker.async-agent.kit.version=$ASYNC_AGENT_KIT_VERSION`)
	assert.Contains(t, string(recipe), "xx-verify --static /docker-agent")
	assert.Contains(t, string(recipe), `ENTRYPOINT ["/opt/async-agent/launch.sh"]`)
	assert.Contains(t, string(recipe), `COPY kit/hackerspace.yaml /opt/async-agent/hackerspace.yaml`)
	assert.Contains(t, string(recipe), `COPY --from=layout --chown=agent:agent --chmod=0700 /layout/home/agent/.config/ /home/agent/.config/`)
	assert.Contains(t, string(recipe), `COPY --chown=agent:agent --chmod=0600 kit/user-config.yaml /home/agent/.config/cagent/config.yaml`)
	assert.NotContains(t, string(recipe), "DOCKER_AGENT_CONFIG_DIR")
	assert.NotContains(t, string(recipe), "COPY . ")
	ignore, err := os.ReadFile(kitPath(t, "async-agent.dockerfile.dockerignore"))
	require.NoError(t, err)
	assert.Contains(t, string(ignore), "!kit/hackerspace.yaml\n")
	assert.Contains(t, string(ignore), "!kit/user-config.yaml\n")
	assert.Contains(t, string(recipe), "/layout/home/agent/.config/cagent/hooks.d /layout/home/agent/.cagent")
	assert.Contains(t, string(recipe), "COPY --chown=agent:agent --chmod=0600 kit/context-hooks.yaml /home/agent/.config/cagent/hooks.d/async-agent-context.yaml")
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
