package app

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// stubProvider satisfies the model validity check in NewLocalRuntime.
type stubProvider struct{}

func (stubProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/mock-model") }
func (stubProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &emptyStubStream{}, nil
}

type emptyStubStream struct{}

func (*emptyStubStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, io.EOF
}
func (*emptyStubStream) Close()              {}
func (stubProvider) BaseConfig() base.Config { return base.Config{} }
func (stubProvider) MaxTokens() int          { return 0 }

// Reload flow: a session loaded from the store has no in-memory tree; the App
// must rebuild it through the runtime, which adopts resumable subagents as
// idle sessions rather than marking them stopped.
func TestReplaceSessionHydratesSubagentTree(t *testing.T) {
	t.Parallel()

	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer store.(*session.SQLiteSessionStore).Close()

	// Persisted state from a previous process: session, child sub-session,
	// and the subagent tree pointing at it.
	sess := session.New(session.WithID("019d500e-aaaa-bbbb-cccc-000000000000"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	childSess := session.New(session.WithID("child-sess-1"))
	childSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), childSess))
	stoppedSess := session.New(session.WithID("child-sess-stopped"))
	stoppedSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), stoppedSess))

	rootID := subagent.SessionRootID(sess.ID)
	snap := subagent.Snapshot{
		Root: rootID,
		Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
			Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: childSess.ID, State: subagent.NodeIdle}},
				{Node: subagent.Node{ID: "55d0f", Agent: "planner", Parent: rootID, SessionID: stoppedSess.ID, State: subagent.NodeStopped}},
			},
		}},
	}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, snap))

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(stubProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt", agent.WithModel(stubProvider{})),
	))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })

	// Reload: GetSession returns the row without the tree.
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Nil(t, loaded.GetSubagentTree())

	a := New(t.Context(), owner.Runtime(), session.New(), runtime.SessionBinding{}, WithRuntimeServices(rt))
	a.ReplaceSession(t.Context(), loaded)

	got := a.Session().GetSubagentTree()
	require.NotNil(t, got, "ReplaceSession must hydrate the subagent tree from the store")
	var idleChild, stoppedChild *subagent.NodeSnapshot
	for i := range got.Nodes {
		if got.Nodes[i].Node.ID == rootID {
			require.Len(t, got.Nodes[i].Children, 2)
			for j := range got.Nodes[i].Children {
				child := &got.Nodes[i].Children[j]
				switch child.Node.ID {
				case "77c88":
					idleChild = child
				case "55d0f":
					stoppedChild = child
				}
			}
		}
	}
	require.NotNil(t, idleChild)
	assert.Equal(t, "planner", idleChild.Node.Agent)
	assert.Equal(t, subagent.NodeIdle, idleChild.Node.State, "resumable subagents are adopted as idle, not stopped")
	require.NotNil(t, stoppedChild)
	assert.Equal(t, "planner", stoppedChild.Node.Agent)
	assert.Equal(t, subagent.NodeStopped, stoppedChild.Node.State, "stopped subagents remain stopped on reload")

	// Restore again after the runtime already tracks this root ID. The newly
	// loaded Session is a distinct projection and must receive the live tree.
	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotSame(t, loaded, reloaded)
	require.Nil(t, reloaded.GetSubagentTree())

	a.ReplaceSession(t.Context(), reloaded)

	currentTree := a.Session().GetSubagentTree()
	require.NotNil(t, currentTree)
	require.Equal(t, got.Root, currentTree.Root)
	require.Equal(t, got.Nodes, currentTree.Nodes, "an already-tracked session tree must hydrate a distinct same-ID session")
}
