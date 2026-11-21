package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type manualEffortHarness struct{}

func (manualEffortHarness) Name() string { return "offline" }
func (manualEffortHarness) Run(_ context.Context, _ string, emit func(harness.Event)) error {
	emit(harness.Event{Type: harness.EventResult, Result: "ok"})
	return nil
}
func (p manualEffortHarness) Resume(ctx context.Context, _, prompt string, emit func(harness.Event)) error {
	return p.Run(ctx, prompt, emit)
}

func TestManualHarnessEffortReachesExecutionAndRestore(t *testing.T) {
	for _, revision := range []bool{false, true} {
		name := "local"
		if revision {
			name = "team revision"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var mu sync.Mutex
			var efforts []string
			store := session.NewInMemorySessionStore()
			modelCfg := ModelSwitcherConfig{EnvProvider: environment.NewMapEnvProvider(nil)}
			build := func() *LocalRuntime {
				root := agent.New("root", "offline", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code", Effort: "low"}))
				tm := team.New(team.WithAgents(root))
				opts := []Opt{WithSessionStore(store), WithSessionCompaction(false), WithModelStore(emptyCatalogStore{}), WithModelSwitcherConfig(&modelCfg), WithHarnessFactory(func(cfg *latest.HarnessConfig) (harness.Provider, error) {
					mu.Lock()
					efforts = append(efforts, cfg.Effort)
					mu.Unlock()
					return manualEffortHarness{}, nil
				})}
				if revision {
					env, err := NewTeamEnvironment("effort-r1", "effort-source", "effort-baseline", "offline", tm, modelCfg, func(context.Context) error { return nil })
					require.NoError(t, err)
					revisions, err := NewTeamEnvironmentStore(env)
					require.NoError(t, err)
					opts = append(opts, WithTeamEnvironmentStore(revisions))
				}
				rt, err := NewLocalRuntime(ctx, tm, opts...)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, rt.Close()) })
				return rt
			}
			rt := build()
			sess := session.New(session.WithAgentName("root"), session.WithTitle("offline"))
			handle, err := rt.CreateSession(ctx, sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			_, err = handle.SetThinkingLevel(ctx, effort.Max)
			require.NoError(t, err)
			submit := func(h SessionHandle) {
				t.Helper()
				obs, err := h.Observe(ctx, ObserveOptions{Buffer: 128})
				require.NoError(t, err)
				defer obs.Cancel()
				roster := obs.Primary().Presentation[1].(*TeamInfoEvent)
				require.Equal(t, "max", roster.AvailableAgents[0].Thinking)
				require.Equal(t, "max", roster.AvailableAgents[0].ThinkingLevel)
				turn, err := h.Submit(ctx, TurnInput{Content: "go"})
				require.NoError(t, err)
				for e := range obs.Events {
					if e.TurnID != turn.TurnID {
						continue
					}
					if roster, ok := e.Event.(*TeamInfoEvent); ok {
						require.Equal(t, "max", roster.AvailableAgents[0].Thinking)
						require.Equal(t, "max", roster.AvailableAgents[0].ThinkingLevel)
					}
					if _, ok := e.Event.(*StreamStoppedEvent); ok {
						break
					}
				}
				require.NoError(t, h.AwaitTurn(ctx, turn.TurnID))
			}
			submit(handle)
			submit(handle)
			loaded, err := store.GetSession(ctx, sess.ID)
			require.NoError(t, err)
			require.NoError(t, rt.Close())
			restored, err := build().CreateSession(ctx, loaded, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			submit(restored)
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, []string{"max", "max", "max"}, efforts)
		})
	}
}
