package snapshot_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

const simpleYAML = "agents:\n  root:\n    model: openai/test-model\n    instruction: original\n"

func write(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
}

func digest(t *testing.T, m *snapshot.Manifest) string {
	t.Helper()
	d, err := m.Digest()
	require.NoError(t, err)
	return d
}

func freeze(t *testing.T, source config.Source, opts snapshot.Options) *snapshot.Manifest {
	t.Helper()
	m, err := snapshot.Snapshot(t.Context(), source, opts)
	require.NoError(t, err)
	return m
}

func TestDependencyFingerprintAndImmutableRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "root.yaml")
	instruction := filepath.Join(dir, "instructions.md")
	external := filepath.Join(dir, "external.yaml")
	rootYAML := `agents:
  root:
    model: openai/test-model
    instruction_file: instructions.md
    sub_agents: [reviewer:example/review]
    handoffs: [reviewer:example/review]
    force_handoff: reviewer:example/review
`
	write(t, root, rootYAML)
	write(t, instruction, "original instructions")
	write(t, external, simpleYAML)
	reads := 0
	opts := snapshot.Options{
		Startup: snapshot.Startup{Workspace: dir, SourceKey: "original-catalog-key"},
		Resolve: func(ref string, _ environment.Provider) (config.Source, error) {
			reads++
			require.Equal(t, "example/review", ref)
			return config.NewFileSource(external), nil
		},
	}
	m := freeze(t, config.NewFileSource(root), opts)
	require.Equal(t, 1, reads, "duplicate graph edges resolve once")
	original := digest(t, m)
	for _, tc := range []struct{ name, path, data, restore string }{
		{"root", root, strings.Replace(rootYAML, "openai/test-model", "openai/other", 1), rootYAML},
		{"instruction", instruction, "updated instructions", "original instructions"},
		{"external", external, strings.Replace(simpleYAML, "original", "changed", 1), simpleYAML},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, tc.path, tc.data)
			changed := freeze(t, config.NewFileSource(root), opts)
			assert.NotEqual(t, original, digest(t, changed))
			assert.Equal(t, original, digest(t, m))
			write(t, tc.path, tc.restore)
		})
	}
	data, err := m.Marshal()
	require.NoError(t, err)
	for _, path := range []string{root, instruction, external} {
		require.NoError(t, os.Remove(path))
	}
	restored, err := snapshot.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, original, digest(t, restored))
	assert.Equal(t, root, restored.FrozenSource().Name())
	assert.Equal(t, dir, restored.FrozenSource().ParentDir())
	assert.Equal(t, "original-catalog-key", restored.Startup().SourceKey)
	for range 2 {
		cfg, err := config.Load(t.Context(), restored.FrozenSource())
		require.NoError(t, err)
		assert.Equal(t, "original instructions", cfg.Agents[0].Instruction)
		source, err := restored.SourceResolver("example/review", nil)
		require.NoError(t, err)
		cfg, err = config.Load(t.Context(), source)
		require.NoError(t, err)
		assert.Equal(t, "original", cfg.Agents[0].Instruction)
	}
	_, err = restored.SourceResolver("example/unknown", nil)
	require.ErrorContains(t, err, "absent")
	// Callers cannot mutate immutable source bytes or startup lists/maps.
	bytes, err := restored.FrozenSource().Read(t.Context())
	require.NoError(t, err)
	bytes[0] = '!'
	assert.Equal(t, original, digest(t, restored))
}

func TestHCLAndEncryptedSourceIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "root.hcl")
	write(t, filepath.Join(dir, "prompt.md"), "HCL file contents")
	write(t, path, `agent "root" {
  model = "openai/test-model"
  instruction = file("prompt.md")
}`)
	m := freeze(t, hcl.NewSource(config.NewFileSource(path)), snapshot.Options{Startup: snapshot.Startup{Workspace: dir}})
	before := digest(t, m)
	write(t, filepath.Join(dir, "prompt.md"), "updated HCL file contents")
	assert.NotEqual(t, before, digest(t, freeze(t, hcl.NewSource(config.NewFileSource(path)), snapshot.Options{Startup: snapshot.Startup{Workspace: dir}})))
	require.NoError(t, os.Remove(filepath.Join(dir, "prompt.md")))
	cfg, err := config.Load(t.Context(), m.FrozenSource())
	require.NoError(t, err)
	assert.Equal(t, "HCL file contents", cfg.Agents[0].Instruction)
	assert.Equal(t, path, m.FrozenSource().Name())

	url := "https://example.com/agent.hcl"
	source := encryptedSource{Source: config.NewBytesSource(url, []byte(simpleYAML)), encrypted: "opaque-ciphertext"}
	opts := snapshot.Options{Startup: snapshot.Startup{Workspace: dir}}
	m = freeze(t, source, opts)
	data, err := m.Marshal()
	require.NoError(t, err)
	m, err = snapshot.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, url, m.FrozenSource().Name())
	assert.Empty(t, m.FrozenSource().ParentDir())
	assert.Equal(t, "opaque-ciphertext", m.FrozenSource().(config.EncryptedConfigSource).EncryptedConfig())
	source.encrypted = "rotated-ciphertext"
	assert.Equal(t, digest(t, m), digest(t, freeze(t, source, opts)))
}

type encryptedSource struct {
	config.Source
	encrypted string
}

func (s encryptedSource) EncryptedConfig() string { return s.encrypted }

func TestDynamicContextAndCredentialsExcluded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "root.yaml")
	write(t, root, simpleYAML+"    add_prompt_files: [context.md]\n    skills: [local]\n")
	prompt, skill, env := filepath.Join(dir, "context.md"), filepath.Join(dir, "SKILL.md"), filepath.Join(dir, ".env")
	for _, path := range []string{prompt, skill, env} {
		write(t, path, "initial")
	}
	opts := snapshot.Options{Startup: snapshot.Startup{Workspace: dir, Runtime: config.Config{EnvFiles: []string{".env"}}}}
	m := freeze(t, config.NewFileSource(root), opts)
	for _, path := range []string{prompt, skill, env} {
		write(t, path, "changed-secret-not-in-manifest")
	}
	m2 := freeze(t, config.NewFileSource(root), opts)
	assert.Equal(t, digest(t, m), digest(t, m2))
	data, err := m2.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "changed-secret-not-in-manifest")
	assert.Equal(t, []string{env}, m2.Startup().Runtime.EnvFiles)
	cfg, err := config.Load(t.Context(), m2.FrozenSource())
	require.NoError(t, err)
	assert.Equal(t, []string{"context.md"}, cfg.Agents[0].AddPromptFiles)
	assert.Equal(t, []string{"local"}, cfg.Agents[0].Skills.Sources)
}

func TestFlavorsAndSharedDefinitionsAppliedOnlyOnce(t *testing.T) {
	t.Parallel()
	source := config.NewBytesSource("agent.yaml", []byte(`version: "15"
toolsets:
  thoughts:
    type: think
agents:
  root:
    model: openai/test-model
    instruction: base
    use_toolsets: [thoughts]
flavors:
  extra:
    agents:
      root:
        instruction+: [extra]
        toolsets+: [{type: filesystem}]
`))
	opts := snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir(), Runtime: config.Config{Flavors: []string{"extra"}}}}
	m := freeze(t, source, opts)
	for range 2 {
		cfg, err := config.Load(t.Context(), m.FrozenSource(), config.WithFlavors(m.Startup().Runtime.Flavors...))
		require.NoError(t, err)
		assert.Equal(t, latest.Version, cfg.Version)
		assert.Equal(t, "base\n\nextra", cfg.Agents[0].Instruction)
		assert.Len(t, cfg.Agents[0].Toolsets, 2)
	}
}

func TestStartupIdentityAndCopyIsolation(t *testing.T) {
	t.Parallel()
	opts := snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir(), SourceKey: "team", ModelOverrides: []string{"openai/a", "openai/b"}, Runtime: config.Config{Flavors: []string{"first", "second"}, HookStop: []string{"echo stop"}}}}
	source := config.NewBytesSource("team.yaml", []byte(simpleYAML))
	m := freeze(t, source, opts)
	before := digest(t, m)
	copy := m.Startup()
	copy.ModelOverrides[0] = "mutated"
	copy.Runtime.HookStop[0] = "mutated"
	opts.Startup.ModelOverrides[0] = "also mutated"
	assert.Equal(t, before, digest(t, m))
	for _, tc := range []struct {
		name   string
		change func(*snapshot.Startup)
	}{
		{"model order", func(s *snapshot.Startup) {
			s.ModelOverrides[0], s.ModelOverrides[1] = s.ModelOverrides[1], s.ModelOverrides[0]
		}},
		{"flavor order", func(s *snapshot.Startup) {
			s.Runtime.Flavors[0], s.Runtime.Flavors[1] = s.Runtime.Flavors[1], s.Runtime.Flavors[0]
		}},
		{"hook", func(s *snapshot.Startup) { s.Runtime.HookStop[0] = "different" }},
		{"gateway", func(s *snapshot.Startup) { s.Runtime.ModelsGateway = "https://gateway.example" }},
		{"env path", func(s *snapshot.Startup) { s.Runtime.EnvFiles = []string{"different.env"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startup := m.Startup()
			tc.change(&startup)
			assert.NotEqual(t, before, digest(t, freeze(t, source, snapshot.Options{Startup: startup})))
		})
	}
}

func TestGraphGuardsAndFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		limit   int
		resolve bool
		fail    bool
		cycle   bool
		want    string
	}{
		{"missing resolver", 0, false, false, false, "require a snapshot resolver"},
		{"fetch failure", 0, true, true, false, "offline"},
		{"cycle", 0, true, false, true, "cycle"},
		{"source limit", 2, true, false, false, "source limit"},
		{"depth limit", 0, true, false, false, "depth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := config.NewBytesSource("root", []byte(simpleYAML+"    sub_agents: [example/next]\n"))
			opts := snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir()}, MaxSources: tc.limit}
			count := 0
			if tc.resolve {
				opts.Resolve = func(string, environment.Provider) (config.Source, error) {
					if tc.fail {
						return nil, errors.New("offline")
					}
					if tc.cycle {
						return root, nil
					}
					count++
					return config.NewBytesSource(fmt.Sprintf("node%d", count), []byte(simpleYAML+fmt.Sprintf("    handoffs: [example/next%d]\n", count))), nil
				}
			}
			_, err := snapshot.Snapshot(t.Context(), root, opts)
			require.ErrorContains(t, err, tc.want)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := snapshot.Snapshot(ctx, config.NewBytesSource("root", []byte(simpleYAML)), snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir()}})
	require.ErrorIs(t, err, context.Canceled)
}

// No network SDK is linked or called: the actual loader gets this minimal
// provider factory, and completion is forbidden. This proves model policies are
// rebuilt from pre-override snapshots for each subsequent runtime.
type fakeModel struct{ base.Config }

func (*fakeModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("unexpected completion request")
}

func TestDeferredOverridesAcrossRuntimeLoads(t *testing.T) {
	t.Parallel()
	source := config.NewBytesSource("team.yaml", []byte(`models:
  serial:
    provider: openai
    model: original-serial
    parallel_tool_calls: false
  parallel:
    provider: openai
    model: original-parallel
    parallel_tool_calls: true
agents:
  root:
    model: serial
    instruction: test
  worker:
    model: parallel
    instruction: test
`))
	m := freeze(t, source, snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir(), ModelOverrides: []string{"openai/first", "openai/replacement"}}})
	calls := 0
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		calls++
		return &fakeModel{Config: base.Config{ModelConfig: *cfg}}, nil
	}})
	store, err := modelsdev.NewStore(
		modelsdev.WithCache(filepath.Join(t.TempDir(), "models.json")),
		modelsdev.WithFetcher(func(context.Context, string) (*modelsdev.Database, string, error) {
			return nil, "", errors.New("offline deterministic test")
		}),
	)
	require.NoError(t, err)
	for range 2 {
		startup := m.Startup()
		run := &config.RuntimeConfig{Config: startup.Runtime, ModelsDevStoreOverride: store, EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=not-a-real-key"})}
		result, err := teamloader.LoadWithConfig(t.Context(), m.FrozenSource(), run,
			teamloader.WithProviderRegistry(registry), teamloader.WithModelOverrides(startup.ModelOverrides), teamloader.WithSourceResolver(m.SourceResolver))
		require.NoError(t, err)
		for name, expected := range map[string]bool{"root": false, "worker": true} {
			a, err := result.Team.Agent(name)
			require.NoError(t, err)
			models := a.ConfiguredModels()
			require.Len(t, models, 1)
			cfg := models[0].BaseConfig().ModelConfig
			assert.Equal(t, "replacement", cfg.Model)
			require.NotNil(t, cfg.ParallelToolCalls)
			assert.Equal(t, expected, *cfg.ParallelToolCalls)
		}
		assert.Nil(t, result.Models["openai/replacement"].ParallelToolCalls, "shared target remains authoritative")
	}
	assert.Equal(t, 4, calls)
}

func TestRestoreRejectsTamperedGraphAndLiveFileReads(t *testing.T) {
	t.Parallel()
	m := freeze(t, config.NewBytesSource("root", []byte(simpleYAML)), snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir()}})
	data, err := m.Marshal()
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"missing root", func(doc map[string]any) { delete(doc["sources"].(map[string]any), "$root") }, "no root"},
		{"unsupported version", func(doc map[string]any) { doc["version"] = 100 }, "unsupported"},
		{"unlisted reference", func(doc map[string]any) {
			node := doc["sources"].(map[string]any)["$root"].(map[string]any)
			node["yaml"] = simpleYAML + "    sub_agents: [example/unknown]\n"
		}, "references do not match"},
		{"instruction file", func(doc map[string]any) {
			node := doc["sources"].(map[string]any)["$root"].(map[string]any)
			node["parent_dir"] = t.TempDir()
			node["yaml"] = strings.Replace(simpleYAML, "instruction: original", "instruction_file: secret", 1)
		}, "instruction_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			require.NoError(t, json.Unmarshal(data, &doc))
			tc.change(doc)
			changed, err := json.Marshal(doc)
			require.NoError(t, err)
			_, err = snapshot.Parse(changed)
			require.ErrorContains(t, err, tc.want)
		})
	}
	_, err = snapshot.Parse(append(data, []byte("{}")...))
	require.ErrorContains(t, err, "trailing")
}

func TestEveryExternalEdgeKindAndNestedDependency(t *testing.T) {
	t.Parallel()
	root := config.NewBytesSource("root", []byte(simpleYAML+`    sub_agents: [example/sub]
    handoffs: [example/handoff]
    force_handoff: example/forced
`))
	contents := map[string]string{
		"example/sub":     simpleYAML,
		"example/handoff": simpleYAML,
		"example/forced":  simpleYAML + "    handoffs: [example/nested]\n",
		"example/nested":  simpleYAML,
	}
	resolved := map[string]int{}
	opts := snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir()}, Resolve: func(ref string, _ environment.Provider) (config.Source, error) {
		resolved[ref]++
		return config.NewBytesSource(ref, []byte(contents[ref])), nil
	}}
	m := freeze(t, root, opts)
	for ref := range contents {
		assert.Equal(t, 1, resolved[ref])
		_, err := m.SourceResolver(ref, nil)
		require.NoError(t, err)
	}
	contents["example/nested"] = strings.Replace(simpleYAML, "original", "new nested instruction", 1)
	assert.NotEqual(t, digest(t, m), digest(t, freeze(t, root, opts)))
}

func TestDepthLimitIncludesAlreadyVisitedSubgraphs(t *testing.T) {
	t.Parallel()
	// The shared leaf is discovered first via a short path. Reusing it at
	// depth ten must still account for its own external dependency.
	root := config.NewBytesSource("root", []byte(simpleYAML+"    sub_agents: [example/shared, example/chain0]\n"))
	opts := snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir()}, Resolve: func(ref string, _ environment.Provider) (config.Source, error) {
		content := simpleYAML
		switch ref {
		case "example/shared":
			content += "    sub_agents: [example/leaf]\n"
		case "example/leaf":
		default:
			var i int
			_, err := fmt.Sscanf(ref, "example/chain%d", &i)
			require.NoError(t, err)
			next := fmt.Sprintf("example/chain%d", i+1)
			if i == 8 {
				next = "example/shared"
			}
			content += fmt.Sprintf("    sub_agents: [%s]\n", next)
		}
		return config.NewBytesSource(ref, []byte(content)), nil
	}}
	_, err := snapshot.Snapshot(t.Context(), root, opts)
	require.ErrorContains(t, err, "depth")
}
