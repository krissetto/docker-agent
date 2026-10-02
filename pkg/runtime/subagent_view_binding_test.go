package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSessionViewWorkspaceAliasPreservesProvenanceAndBindsRoot(t *testing.T) {
	workspace := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(workspace, alias))
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(cwd, workspace)
	require.NoError(t, err)
	for _, path := range []string{alias, relative} {
		t.Run(path, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			root := session.New(session.WithID("aliased-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
			root.WorkingDir = path
			require.NoError(t, store.AddSession(t.Context(), root))
			rt, owner := coordinationRuntime(t, store, coordinationReply("done"), coordinationReply("child"), WithWorkingDir(workspace))
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
			require.NoError(t, err)
			committed, err := prepared.Commit(t.Context())
			require.NoError(t, err)
			require.Equal(t, path, committed.Info.WorkingDir)
			stored, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			require.Equal(t, path, stored.WorkingDir)
			driver, ok := rt.sessionDrivers.Lookup(root.ID)
			require.True(t, ok)
			driver.mu.Lock()
			started, settled := driver.startedCallbacksLocked(), driver.settledCallbacksLocked()
			driver.mu.Unlock()
			require.Len(t, started, 1)
			require.Len(t, settled, 1)
			started[0]()
			node, ok := rt.subagents.tree.Node(subagent.SessionRootID(root.ID))
			require.True(t, ok)
			require.Equal(t, subagent.NodeRunning, node.State)
			driver.mu.Lock()
			driver.lastError = "view turn failed"
			driver.mu.Unlock()
			settled[0]()
			node, _ = rt.subagents.tree.Node(subagent.SessionRootID(root.ID))
			require.Equal(t, subagent.NodeFailed, node.State)
		})
	}
	require.False(t, sameWorkspace(workspace, t.TempDir()))
}
