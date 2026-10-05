package app

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type idleTreeProvider struct{}

func (idleTreeProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/tree") }
func (idleTreeProvider) BaseConfig() base.Config { return base.Config{} }
func (idleTreeProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return idleTreeStream{}, nil
}

type idleTreeStream struct{}

func (idleTreeStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, io.EOF
}
func (idleTreeStream) Close() {}

// The kit runs the TUI against --managed-api: child navigation and subtree
// control must work over the real session HTTP API, not in-process casts.
func TestRemoteViewResolvesOpensAndStopsDescendantOverSessionAPI(t *testing.T) {
	store := session.NewInMemorySessionStore()
	dir := t.TempDir()
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(idleTreeProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(idleTreeProvider{})),
	)), runtime.WithSessionStore(store), runtime.WithWorkingDir(dir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	rootSession := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	_, err = owner.Runtime().CreateSession(t.Context(), rootSession, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	serverCtx, stopServer := context.WithCancel(t.Context())
	t.Cleanup(stopServer)
	listener, err := (&net.ListenConfig{}).Listen(serverCtx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	api := server.NewWithManager(server.NewSessionManager(serverCtx, nil, store, 0, nil, server.WithSessionRuntime(owner.Runtime())), "")
	go func() { _ = api.Serve(serverCtx, listener) }()
	client, err := runtime.NewClient("http://" + listener.Addr().String())
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)

	view := New(t.Context(), transport, rootSession.Clone(), runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}))
	require.NotNil(t, view.SessionHandle())
	require.False(t, runtime.IsLocalSessionHandle(view.SessionHandle()))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	t.Cleanup(view.Close)
	trees := make(chan *runtime.SubagentTreeEvent, 64)
	ready := make(chan struct{})
	go view.Subscribe(ctx, func(msg any) {
		if tree, ok := msg.(*runtime.SubagentTreeEvent); ok {
			select {
			case trees <- tree:
			default:
			}
		}
	}, SubscribeOptions{Ready: ready})
	<-ready
	view.Start(ctx)

	// Spawn while the client attaches: topology arrives only through observation.
	child, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithAgentName("worker"), session.WithWorkingDir(dir)), runtime.SessionBinding{AgentName: "worker", ParentSessionID: rootSession.ID})
	require.NoError(t, err)
	node, ok := local.SubagentNodeForSession(child.ID())
	require.True(t, ok)

	var target SubagentTarget
	require.Eventually(t, func() bool {
		target, ok = view.ResolveSubagentTarget(string(node))
		return ok
	}, 5*time.Second, 10*time.Millisecond, "remote view learns the spawned child by node ID")
	assert.Equal(t, child.ID(), target.SessionID)
	bySession, ok := view.ResolveSubagentTarget(child.ID())
	require.True(t, ok)
	assert.Equal(t, node, bySession.NodeID)
	resolvedNode, ok := view.SubagentNodeForSession(child.ID())
	require.True(t, ok)
	assert.Equal(t, node, resolvedNode)
	select {
	case tree := <-trees:
		_, found := findNode(tree.Snapshot.Nodes, func(n subagent.Node) bool { return n.SessionID == child.ID() })
		assert.True(t, found, "remote tree events carry the observed child")
	case <-time.After(5 * time.Second):
		t.Fatal("remote view published no subagent tree")
	}

	prepared, err := transport.PrepareSessionView(t.Context(), target.SessionID)
	require.NoError(t, err)
	t.Cleanup(prepared.Abort)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	childView, err := NewResolvedFromTemplate(t.Context(), transport, committed, view)
	require.NoError(t, err)
	t.Cleanup(childView.Close)
	assert.Equal(t, child.ID(), childView.SessionHandle().ID())
	assert.Equal(t, rootSession.ID, childView.Session().ParentID, "remote views retain canonical child ancestry")
	require.NotNil(t, childView.AttachedSubagent())
	assert.Equal(t, rootSession.ID, childView.AttachedSubagent().Session.ParentID)

	// Use subagents is the canonical tree policy: the child view reads the root's.
	require.True(t, view.CanSetDelegationPolicy())
	enabled, err := view.DelegationPolicy(t.Context())
	require.NoError(t, err)
	assert.True(t, enabled, "absent override reads the owner's default")
	require.NoError(t, view.SetDelegationPolicy(t.Context(), false))
	enabled, err = childView.DelegationPolicy(t.Context())
	require.NoError(t, err)
	assert.False(t, enabled, "descendants inherit the root's policy")
	require.NoError(t, childView.SetDelegationPolicy(t.Context(), true))
	enabled, err = view.DelegationPolicy(t.Context())
	require.NoError(t, err)
	assert.True(t, enabled, "a child view changes its canonical root's policy")

	require.True(t, view.CanStopSubtree(), "remote owner advertises portable stop-tree")
	require.NoError(t, view.StopSubtree(t.Context(), target))
	require.Eventually(t, func() bool {
		found, ok := local.SubagentTree().Node(node)
		return ok && found.State == subagent.NodeStopped
	}, 5*time.Second, 10*time.Millisecond, "stop reaches the canonical owner over HTTP")
}

// The saved local preference is only the owner's default: the session-tree
// policy overrides it for that tree without rewriting the default.
func TestLocalDelegationPolicyOverridesDefaultWithoutChangingIt(t *testing.T) {
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(idleTreeProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(idleTreeProvider{})),
	)), runtime.WithSessionStore(session.NewInMemorySessionStore()), runtime.WithUseSubagents(false))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	view := New(t.Context(), owner.Runtime(), session.New(session.WithAgentName("root")), runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(local))
	t.Cleanup(view.Close)
	require.True(t, view.CanSetDelegationPolicy())
	enabled, err := view.DelegationPolicy(t.Context())
	require.NoError(t, err)
	assert.False(t, enabled, "no tree override: the local default applies")
	require.NoError(t, view.SetDelegationPolicy(t.Context(), true))
	enabled, err = view.DelegationPolicy(t.Context())
	require.NoError(t, err)
	assert.True(t, enabled)
	assert.False(t, local.UseSubagents(), "the saved default is untouched")
}
