package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

// newThinkingPersistenceRuntime builds a runtime whose YAML pins gpt-5 to low
// effort, so any level other than low proves a session-scoped choice survived.
func newThinkingPersistenceRuntime(t *testing.T, store session.Store) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "test", agent.WithModel(newConfigProvider(latest.ModelConfig{
		Provider:       "openai",
		Model:          "gpt-5",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "low"},
	})))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithSessionStore(store),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })
	return rt
}

func TestManualThinkingLevelSurvivesReopenedSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	rt := newThinkingPersistenceRuntime(t, store)
	sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	applied, err := handle.(*sessionHandle).SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	require.Equal(t, effort.High, applied)
	require.NoError(t, rt.shutdownSessions(t.Context()))
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	loaded, err := reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)

	rt2 := newThinkingPersistenceRuntime(t, reopened)
	restored, err := rt2.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Equal(t, effort.High, restored.(*sessionHandle).CurrentThinkingLevel(t.Context()), "a reattached session must keep the level the user picked, not the YAML default")
}

func TestManualThinkingLevelSurvivesActiveAgentRoundTrip(t *testing.T) {
	yamlLow := &latest.ThinkingBudget{Effort: "low"}
	root := agent.New("root", "test", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: yamlLow})))
	worker := agent.New("worker", "test", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: yamlLow})))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithSessionCompaction(false),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })

	handle, err := rt.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)
	_, err = h.SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)

	require.NoError(t, h.SwitchActiveAgent(t.Context(), "worker"))
	require.NoError(t, h.SwitchActiveAgent(t.Context(), "root"))
	assert.Equal(t, effort.High, h.CurrentThinkingLevel(t.Context()), "switching away and back must not drop the root's manual level")
}

func TestChangingModelClearsManualThinkingLevel(t *testing.T) {
	r := newModelDisplayRuntime(t)
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)
	_, err = h.SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	require.Equal(t, string(effort.High), h.driver.session().AgentThinkingLevel("root"))

	require.NoError(t, h.SetModel(t.Context(), "fast"))
	assert.Empty(t, h.driver.session().AgentThinkingLevel("root"), "an explicit model change starts from the new model's default effort")
}

func newHarnessThinkingRuntime(t *testing.T, store session.Store) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "test", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithSessionStore(store),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{}),
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })
	return rt
}

func TestManualHarnessEffortSurvivesReopenedSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	rt := newHarnessThinkingRuntime(t, store)
	sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	applied, err := handle.(*sessionHandle).SetThinkingLevel(t.Context(), effort.Max)
	require.NoError(t, err)
	require.Equal(t, effort.Max, applied)
	require.NoError(t, rt.shutdownSessions(t.Context()))
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	loaded, err := reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)

	rt2 := newHarnessThinkingRuntime(t, reopened)
	restored, err := rt2.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Equal(t, effort.Max, restored.(*sessionHandle).CurrentThinkingLevel(t.Context()), "a reattached harness session must keep its chosen effort")
}

// A stored value outside the harness vocabulary (a model-only level such as
// "none") is not a harness choice: the CLI default must apply, not that value.
func TestStoredNonHarnessLevelIsIgnoredByHarness(t *testing.T) {
	r := newHarnessThinkingRuntime(t, session.NewInMemorySessionStore())
	sess := session.New(session.WithAgentName("root"))
	sess.SetAgentThinkingLevel("root", string(effort.None))
	handle, err := r.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Equal(t, effort.Level(""), handle.(*sessionHandle).CurrentThinkingLevel(t.Context()), "a model-only stored level must not become a harness effort")
}

// A team revision published mid-chat is pinned by the next turn through the
// shared resolver; the manual level must survive that rebind.
func TestTeamRevisionRebindKeepsManualThinkingLevel(t *testing.T) {
	t.Run("model agent", func(t *testing.T) {
		newRoot := func() *agent.Agent {
			return agent.New("root", "test", agent.WithModel(newConfigProvider(latest.ModelConfig{
				Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "low"},
			})))
		}
		assertRebindKeepsLevel(t, newRoot, effort.High)
	})
	t.Run("harness agent", func(t *testing.T) {
		newRoot := func() *agent.Agent {
			return agent.New("root", "test", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code"}))
		}
		assertRebindKeepsLevel(t, newRoot, effort.Max)
	})
}

func assertRebindKeepsLevel(t *testing.T, newRoot func() *agent.Agent, level effort.Level) {
	t.Helper()
	modelCfg := ModelSwitcherConfig{
		ProviderRegistry: testProviderRegistry(),
		EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
	}
	build := func(revision string) *TeamEnvironment {
		env, err := NewTeamEnvironment(revision, "thinking-source", "thinking-baseline", "team", team.New(team.WithAgents(newRoot())), modelCfg, func(context.Context) error { return nil })
		require.NoError(t, err)
		return env
	}
	store, err := NewTeamEnvironmentStore(build("thinking-r1"))
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(newRoot())),
		WithSessionCompaction(false),
		WithModelStore(emptyCatalogStore{}),
		WithTeamEnvironmentStore(store),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })

	handle, err := rt.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)
	_, err = h.SetThinkingLevel(t.Context(), level)
	require.NoError(t, err)
	require.Equal(t, level, h.CurrentThinkingLevel(t.Context()))

	require.NoError(t, store.Publish("thinking-source", "thinking-r1", build("thinking-r2")))
	require.NoError(t, h.driver.pinCurrentTeamEnvironment(t.Context()), "a new turn pins the published revision")
	require.Equal(t, "thinking-r2", h.driver.teamEnvironmentSnapshot().RevisionID())
	assert.Equal(t, level, h.CurrentThinkingLevel(t.Context()), "the rebind must re-apply the manual level, not the YAML default")
}
