package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
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
			if wrapped {
				ts = codemode.Wrap(inner)
			}
			toolsets := []tools.ToolSet{ts}
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
				require.NoError(t, h.Release(t.Context()))
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
