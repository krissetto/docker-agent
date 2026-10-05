package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

type releaseOwnerToolset struct {
	mu          sync.Mutex
	owners      []*tools.ResourceOwner
	globalStops int
}

func (*releaseOwnerToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*releaseOwnerToolset) Start(context.Context) error                 { return nil }
func (s *releaseOwnerToolset) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalStops++
	return nil
}

func (s *releaseOwnerToolset) StopResourceOwner(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, tools.ResourceOwnerFromContext(ctx))
	return nil
}

func TestCanonicalReleaseCodeModeOwnerCleanup(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "code-mode"}[wrapped], func(t *testing.T) {
			inner := &releaseOwnerToolset{}
			var ts tools.ToolSet = inner
			jobs := backgroundjobs.New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
			require.NoError(t, jobs.Start(t.Context()))
			t.Cleanup(func() { require.NoError(t, jobs.Stop(context.WithoutCancel(t.Context()))) })
			if wrapped {
				ts = codemode.Wrap(inner, jobs)
			}
			toolsets := []tools.ToolSet{ts}
			if !wrapped {
				toolsets = append(toolsets, jobs)
			}
			a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/model"}), agent.WithToolSets(toolsets...))
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithSessionStore(session.NewInMemorySessionStore()))
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
				defer cancel()
				require.NoError(t, r.shutdownSessions(ctx))
			})
			for range 2 {
				h, err := r.CreateSession(t.Context(), session.New(session.WithID("reused-public-id")), SessionBinding{AgentName: "root"})
				require.NoError(t, err)
				resident, ok := r.sessionDrivers.Lookup(h.ID())
				require.True(t, ok)
				ownerCtx := tools.WithResourceOwner(t.Context(), resident.resourceOwner)
				decls, err := jobs.Tools(ownerCtx)
				require.NoError(t, err)
				callJob := func(name, args string) *tools.ToolCallResult {
					for _, tool := range decls {
						if tool.Name == name {
							result, err := tool.Handler(ownerCtx, tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: args}}, tools.NopRuntime{})
							require.NoError(t, err)
							require.False(t, result.IsError, result.Output)
							return result
						}
					}
					t.Fatal("missing background job tool")
					return nil
				}
				// A finite owned child only; retirement must clear its retained job state.
				callJob(backgroundjobs.ToolNameRunBackgroundJob, `{"cmd":"exec /bin/sleep 2"}`)
				require.Contains(t, callJob(backgroundjobs.ToolNameListBackgroundJobs, `{}`).Output, "ID:")
				require.NoError(t, h.Release(t.Context()))
				require.Contains(t, callJob(backgroundjobs.ToolNameListBackgroundJobs, `{}`).Output, "No background jobs found")
			}
			inner.mu.Lock()
			defer inner.mu.Unlock()
			require.Len(t, inner.owners, 2)
			require.NotNil(t, inner.owners[0])
			require.NotNil(t, inner.owners[1])
			require.NotSame(t, inner.owners[0], inner.owners[1], "admission generations own separate resources")
			require.Zero(t, inner.globalStops, "release must not stop the shared definition")
		})
	}
}
