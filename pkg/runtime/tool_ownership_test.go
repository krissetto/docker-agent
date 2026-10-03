package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestSharedTeamTodoOwnershipAcrossRuntimes(t *testing.T) {
	definition := todotool.New(todotool.WithShared(true))
	a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(definition))
	sharedTeam := team.New(team.WithAgents(a))
	var runtimes []*LocalRuntime
	var roots []SessionHandle
	for range 2 {
		r, err := NewLocalRuntime(t.Context(), sharedTeam, WithSessionStore(session.NewInMemorySessionStore()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		root, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		// The same child ID resolves to a different root in each runtime.
		r.subagents.mu.Lock()
		r.subagents.sessions[root.ID()] = &sessionSubagents{topLevel: true, node: subagent.SessionRootID(root.ID())}
		r.subagents.sessions["child"] = &sessionSubagents{node: "child-node"}
		r.subagents.children["child-node"] = &childRecord{sessionID: "child", parentSession: root.ID()}
		r.subagents.mu.Unlock()
		runtimes = append(runtimes, r)
		roots = append(roots, root)
	}
	for i, r := range runtimes {
		ctx := todotool.WithBindings(httpclient.ContextWithSessionID(t.Context(), "child"), r.todoToolsets)
		all, err := definition.Tools(ctx)
		require.NoError(t, err)
		for _, tool := range all {
			if tool.Name == todotool.ToolNameCreateTodo {
				_, err = tool.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"description":"runtime owned"}`}}, tools.NopRuntime{})
				require.NoError(t, err)
			}
		}
		items, err := roots[i].Todos(t.Context())
		require.NoError(t, err)
		require.Len(t, items, 1)
	}
	// Neither runtime has rebound the reusable definition's storage.
	original, err := definition.Todos(t.Context())
	require.NoError(t, err)
	require.Empty(t, original)
	for _, root := range roots {
		items, err := root.Todos(t.Context())
		require.NoError(t, err)
		require.Len(t, items, 1)
	}
}

type ownershipNotifier struct{ subscribers tools.ChangeSubscribers }

func (*ownershipNotifier) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (n *ownershipNotifier) SubscribeToolsChanged(handler func()) func() {
	return n.subscribers.Subscribe(handler)
}

func TestSharedToolSubscriptionsReleaseWithRuntime(t *testing.T) {
	notifier := &ownershipNotifier{}
	a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(notifier))
	sharedTeam := team.New(team.WithAgents(a))
	var calls [2]atomic.Int32
	var runtimes []*LocalRuntime
	for i := range 2 {
		r, err := NewLocalRuntime(t.Context(), sharedTeam)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		r.OnToolsChanged(func(Event) { calls[i].Add(1) })
		runtimes = append(runtimes, r)
	}
	notifier.subscribers.Notify()
	require.EqualValues(t, 1, calls[0].Load())
	require.EqualValues(t, 1, calls[1].Load())
	runtimes[0].OnToolsChanged(nil)
	notifier.subscribers.Notify()
	require.EqualValues(t, 1, calls[0].Load())
	require.EqualValues(t, 2, calls[1].Load())
	require.NoError(t, runtimes[1].Close())
	require.Eventually(t, func() bool {
		before := calls[1].Load()
		notifier.subscribers.Notify()
		return calls[1].Load() == before
	}, time.Second, time.Millisecond)
}
