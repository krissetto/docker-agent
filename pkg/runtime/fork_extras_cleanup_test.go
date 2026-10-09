package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

type forkCleanupToolset struct {
	mu         sync.Mutex
	owners     []*tools.ResourceOwner
	stops      int
	startErr   error
	cleanupErr error
}

func (*forkCleanupToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *forkCleanupToolset) Start(context.Context) error               { return s.startErr }
func (s *forkCleanupToolset) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return nil
}

func (s *forkCleanupToolset) StopResourceOwner(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, tools.ResourceOwnerFromContext(ctx))
	return s.cleanupErr
}

func TestForkOnlyExtrasRetirement(t *testing.T) {
	for _, mode := range []string{"direct", "code-mode", "failed-start"} {
		for _, retirement := range []string{"release", "child-stop", "shutdown"} {
			t.Run(mode+"/"+retirement, func(t *testing.T) {
				inner := &forkCleanupToolset{}
				if mode == "failed-start" {
					inner.startErr = errors.New("start failed")
				}
				var extra tools.ToolSet = inner
				if mode == "code-mode" {
					extra = codemode.Wrap(inner)
				}
				extra = tools.NewStartable(extra)
				a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/model"}))
				tm := team.New(team.WithAgents(a), team.WithOwnedToolSets(extra.(*tools.StartableToolSet)))
				r, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}), WithSessionStore(session.NewInMemorySessionStore()))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, r.Close()) })
				handles := make([]SessionHandle, 2)
				owners := make([]*tools.ResourceOwner, 2)
				parent := session.New()
				for i := range handles {
					child := session.New(session.WithExtraToolSets([]tools.ToolSet{extra, extra}))
					if retirement == "child-stop" {
						r.subagents.registerChild(parent, "root", subagent.NodeID(child.ID), "root", child)
						d := r.sessionDrivers.Get(child)
						require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; d.closeSettledLocked(); return nil }))
						store := r.sessionStore.(session.CoordinationStore)
						if i == 0 {
							require.NoError(t, r.sessionStore.AddSession(t.Context(), parent))
						}
						rec := r.subagents.children[subagent.NodeID(child.ID)]
						node, ok := r.subagents.tree.Node(subagent.NodeID(child.ID))
						require.True(t, ok)
						node.State = subagent.NodeIdle
						rec.durable = session.ChildRecord{RootSessionID: parent.ID, ParentSessionID: parent.ID, Node: node, Revision: 1}
						require.NoError(t, store.AdmitChild(t.Context(), session.ChildAdmission{Child: child.OwnSnapshot(), Record: rec.durable}))
					}
					handles[i], err = r.CreateSession(t.Context(), child, SessionBinding{AgentName: "root"})
					require.NoError(t, err)
					d, ok := r.sessionDrivers.Lookup(handles[i].ID())
					require.True(t, ok)
					owners[i] = d.resourceOwner
					ctx := tools.WithResourceOwner(t.Context(), owners[i])
					r.skillSubSessionTools(ctx, d.session().Clone(), a, nil)
				}
				switch retirement {
				case "release":
					require.NoError(t, handles[0].Release(t.Context()))
				case "child-stop":
					require.NoError(t, handles[0].(SessionTreeController).StopSubtree(t.Context()))
				case "shutdown":
					require.NoError(t, r.shutdownSessions(t.Context()))
				}
				inner.mu.Lock()
				require.Zero(t, inner.stops, "borrowed team definitions must survive retirement")
				if retirement == "shutdown" {
					require.ElementsMatch(t, owners, inner.owners)
				} else {
					require.Equal(t, []*tools.ResourceOwner{owners[0]}, inner.owners, "one owner cleanup, no sibling cleanup")
				}
				inner.mu.Unlock()
				require.NoError(t, r.shutdownSessions(t.Context()))
				require.NoError(t, tm.StopToolSets(t.Context()))
				require.NoError(t, tm.StopToolSets(t.Context()))
				inner.mu.Lock()
				defer inner.mu.Unlock()
				if mode == "failed-start" {
					require.Zero(t, inner.stops)
				} else {
					require.Equal(t, 1, inner.stops)
				}
			})
		}
	}
}

func TestForkOnlyExtrasCleanupJoinsErrorsAndDedupesAliases(t *testing.T) {
	firstErr, secondErr := errors.New("first cleanup"), errors.New("second cleanup")
	first := &forkCleanupToolset{cleanupErr: firstErr}
	second := &forkCleanupToolset{cleanupErr: secondErr}
	a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/model"}), agent.WithToolSets(first))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	h, err := r.CreateSession(t.Context(), session.New(session.WithExtraToolSets([]tools.ToolSet{first, tools.NewStartable(first), codemode.Wrap(first, second, second), second})), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	err = h.Release(t.Context())
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	first.mu.Lock()
	require.Len(t, first.owners, 1)
	first.cleanupErr = nil
	first.mu.Unlock()
	second.mu.Lock()
	require.Len(t, second.owners, 1)
	second.cleanupErr = nil
	second.mu.Unlock()
	require.NoError(t, h.Release(t.Context()))
	require.NoError(t, r.Close())
}
