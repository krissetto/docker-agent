package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/team"
)

func TestSessionViewPreservesActiveAgentSeparateFromBinding(t *testing.T) {
	store := coordinationSQLite(t)
	active := agent.New("active", "prompt", agent.WithModel(coordinationReply("unused")))
	rootAgent := agent.New("root", "prompt", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
	worker := agent.New("worker", "prompt", agent.WithModel(coordinationReply("unused")))
	makeRuntime := func() SessionRuntimeSupervisor {
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent, worker, active)), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		owner := NewSessionRuntimeSupervisor(rt)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
		return owner
	}
	owner := makeRuntime()
	root := coordinationCreate(t, owner.Runtime(), "view-active-root", "")
	child := coordinationCreate(t, owner.Runtime(), "view-active-child", root.ID())
	require.NoError(t, owner.Shutdown(t.Context()))
	for _, id := range []string{root.ID(), child.ID()} {
		row, err := store.GetSession(t.Context(), id)
		require.NoError(t, err)
		row.AgentName = "active"
		require.NoError(t, store.UpdateSession(t.Context(), row))
	}
	restored := makeRuntime()
	for _, tc := range []struct{ id, binding string }{{root.ID(), "root"}, {child.ID(), "worker"}} {
		t.Run(tc.binding, func(t *testing.T) {
			prepared, err := restored.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), tc.id)
			require.NoError(t, err)
			defer prepared.Abort()
			info := prepared.Info()
			require.Equal(t, tc.binding, info.Binding.AgentName)
			require.Equal(t, "active", info.Session.AgentName)
			if info.Attach != nil {
				require.Equal(t, "active", info.Attach.Agent)
			}
			committed, err := prepared.Commit(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.binding, committed.Info.Binding.AgentName)
			require.Equal(t, "active", committed.Info.ActiveAgentName)
			snapshot, err := committed.SessionHandle.Snapshot(t.Context())
			require.NoError(t, err)
			require.Equal(t, "active", snapshot.AgentName)
			require.Equal(t, tc.binding, snapshot.AttributesSnapshot()[SessionAgentAttribute])
		})
	}
}
