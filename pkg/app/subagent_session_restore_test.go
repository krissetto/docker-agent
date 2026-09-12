package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type sessionTreeRestoreRuntime struct {
	*projectionSessions

	restores int
	snapshot *subagent.Snapshot
}

func (r *sessionTreeRestoreRuntime) RestoreSessionTree(_ context.Context, root *session.Session) error {
	r.restores++
	root.SetSubagentTree(r.snapshot)
	return nil
}

type legacyTreeRestoreServices struct {
	*mockRuntime

	tree     *subagent.Tree
	restores int
}

func (r *legacyTreeRestoreServices) SubagentTree() *subagent.Tree { return r.tree }

func (r *legacyTreeRestoreServices) RestoreSubagentTree(context.Context, *session.Session) (*subagent.Snapshot, error) {
	r.restores++
	return nil, nil
}

func TestReplaceSessionRestoresTreeThroughSessionRuntime(t *testing.T) {
	t.Parallel()

	initial := session.New(session.WithID("initial"), session.WithAgentName("root"))
	loaded := session.New(session.WithID("loaded"), session.WithAgentName("root"))
	snapshot := &subagent.Snapshot{Root: subagent.SessionRootID(loaded.ID)}
	sessions := &sessionTreeRestoreRuntime{
		projectionSessions: &projectionSessions{session: &projectionSession{id: initial.ID}},
		snapshot:           snapshot,
	}
	legacy := &legacyTreeRestoreServices{mockRuntime: &mockRuntime{}, tree: subagent.NewTree()}
	a := New(t.Context(), sessions, initial, runtime.SessionBinding{}, WithRuntimeServices(legacy))

	a.ReplaceSession(t.Context(), loaded)

	require.Equal(t, snapshot, loaded.GetSubagentTree(), "session restoration must populate the loaded session tree")
	assert.Equal(t, 1, sessions.restores)
	assert.Zero(t, legacy.restores, "legacy services must not restore when session capability is available")
}
