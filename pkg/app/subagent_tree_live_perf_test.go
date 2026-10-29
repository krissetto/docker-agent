package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

var liveTreePerfSessionID atomic.Uint64

func liveTreePerfSession(messageCount int) *session.Session {
	sess := session.New(session.WithID(fmt.Sprintf("perf-session-%d", liveTreePerfSessionID.Add(1))), session.WithAgentName("root"))
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":   map[string]any{"type": "string", "description": "synthetic persisted tool schema"},
			"options": map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer"}}},
		},
	}
	definition := tools.Tool{Name: "synthetic_tool", Parameters: schema, OutputSchema: schema}
	for i := range messageCount {
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
			Role:            chat.MessageRoleAssistant,
			Content:         fmt.Sprintf("historical response %05d", i),
			ToolDefinitions: []tools.Tool{definition},
		}))
	}
	return sess
}

func newLiveTreePerfApp(b *testing.B, historyMessages, treeNodes int) *App {
	b.Helper()
	ctx := context.Background()
	rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(
		agent.New("root", "synthetic", agent.WithModel(stubProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "synthetic", agent.WithModel(stubProvider{})),
	)), runtime.WithSessionCompaction(false))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = rt.Close() })
	sess := liveTreePerfSession(historyMessages)
	handle, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: "root"})
	if err != nil {
		b.Fatal(err)
	}
	rootID := subagent.SessionRootID(sess.ID)
	childID := "perf-child-" + sess.ID
	if _, err := rt.CreateSession(ctx, session.New(session.WithID(childID), session.WithAgentName("worker")), runtime.SessionBinding{AgentName: "worker", ParentSessionID: sess.ID}); err != nil {
		b.Fatal(err)
	}
	nodes := make([]subagent.Node, 0, treeNodes)
	levelParents := []subagent.NodeID{rootID}
	var nextLevel []subagent.NodeID
	parentIndex, siblings := 0, 0
	for i := range treeNodes {
		id := subagent.NodeID(fmt.Sprintf("synthetic-node-%05d", i))
		parent := levelParents[parentIndex]
		nodes = append(nodes, subagent.Node{ID: id, Agent: "worker", Parent: parent, State: subagent.NodeIdle})
		nextLevel = append(nextLevel, id)
		siblings++
		if siblings == 10 {
			siblings = 0
			parentIndex++
			if parentIndex == len(levelParents) {
				levelParents, nextLevel = nextLevel, nil
				parentIndex = 0
			}
		}
	}
	if err := rt.SubagentTree().AddSubtree(nodes); err != nil {
		b.Fatal(err)
	}
	app := &App{
		runtime:      rt,
		currentState: sessionState{session: sess, handle: handle, binding: runtime.SessionBinding{AgentName: "root"}},
	}
	return app
}

type treeLivePerfServices struct {
	Services
	tree *subagent.Tree
}

func (s treeLivePerfServices) SubagentTree() *subagent.Tree { return s.tree }

type treeLivePerfHandle struct {
	runtime.SessionHandle
	metadata      runtime.SessionMetadata
	metadataCalls *atomic.Int32
}

func (h treeLivePerfHandle) Metadata() runtime.SessionMetadata {
	if h.metadataCalls != nil {
		h.metadataCalls.Add(1)
	}
	return h.metadata
}

func TestLiveRuntimeTreeSkipsMetadataForLocalHandle(t *testing.T) {
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "synthetic", agent.WithModel(stubProvider{})))))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := liveTreePerfSession(100)
	handle, err := rt.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.True(t, runtime.IsLocalSessionHandle(handle))
	app := &App{runtime: rt, currentState: sessionState{session: sess, handle: handle}}
	require.True(t, app.LiveRuntimeTree())
}

func BenchmarkLiveRuntimeTreeWithLongHistory(b *testing.B) {
	for _, historyMessages := range []int{0, 100, 1000} {
		for _, treeNodes := range []int{100, 1000, 5000} {
			b.Run(fmt.Sprintf("history=%d/tree=%d", historyMessages, treeNodes), func(b *testing.B) {
				app := newLiveTreePerfApp(b, historyMessages, treeNodes)
				for _, tc := range []struct {
					name  string
					check func(*App) bool
				}{
					{name: "metadata-check-baseline", check: legacyLiveRuntimeTree},
					{name: "local-handle-fast-path", check: (*App).LiveRuntimeTree},
				} {
					b.Run(tc.name, func(b *testing.B) {
						b.ReportAllocs()
						b.ResetTimer()
						for range b.N {
							if !tc.check(app) {
								b.Fatal("expected local runtime tree")
							}
						}
					})
				}
			})
		}
	}
}

func legacyLiveRuntimeTree(app *App) bool {
	if handle := app.SessionHandle(); handle != nil && handle.Metadata().Historical {
		return false
	}
	rt, ok := app.runtime.(subagentRuntime)
	return ok && rt.SubagentTree() != nil
}

func legacySubagentNodeForSession(app *App, sessionID string) (subagent.NodeID, bool) {
	if index, ok := app.runtime.(interface {
		SubagentNodeForSession(string) (subagent.NodeID, bool)
	}); ok && legacyLiveRuntimeTree(app) {
		return index.SubagentNodeForSession(sessionID)
	}
	snapshot := app.SubagentTreeSnapshot()
	if snapshot == nil || sessionID == "" {
		return "", false
	}
	node, ok := findNode(snapshot.Nodes, func(n subagent.Node) bool { return n.SessionID == sessionID && n.Parent != "" })
	return node.ID, ok
}

func TestLiveRuntimeTreePreservesHistoricalAndRemoteMetadataChecks(t *testing.T) {
	t.Run("historical local runtime view", func(t *testing.T) {
		store := session.NewInMemorySessionStore()
		archived := session.New(
			session.WithID("archived-root"),
			session.WithAgentName("root"),
			session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}),
		)
		require.NoError(t, store.AddSession(t.Context(), archived))
		rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "synthetic", agent.WithModel(stubProvider{})))), runtime.WithSessionStore(store))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close()) })
		reader := runtime.NewSessionRuntimeSupervisor(rt).Runtime().(runtime.HistoricalViewReader)
		handle, info, err := reader.HistoricalSessionView(t.Context(), archived.ID)
		require.NoError(t, err)
		app := &App{runtime: rt, currentState: sessionState{session: info.Session, handle: handle}}
		require.False(t, runtime.IsLocalSessionHandle(handle))
		require.True(t, handle.Metadata().Historical)
		require.False(t, app.LiveRuntimeTree())
	})

	t.Run("remote metadata fallback", func(t *testing.T) {
		for _, historical := range []bool{false, true} {
			t.Run(fmt.Sprintf("historical=%t", historical), func(t *testing.T) {
				tree := subagent.NewTree()
				services := treeLivePerfServices{tree: tree}
				calls := &atomic.Int32{}
				handle := treeLivePerfHandle{metadata: runtime.SessionMetadata{Historical: historical}, metadataCalls: calls}
				app := &App{runtime: services, currentState: sessionState{handle: handle}}
				require.False(t, runtime.IsLocalSessionHandle(handle))
				require.Equal(t, !historical, app.LiveRuntimeTree())
				require.EqualValues(t, 1, calls.Load())
			})
		}
	})

	t.Run("nil handle", func(t *testing.T) {
		services := treeLivePerfServices{tree: subagent.NewTree()}
		require.True(t, (&App{runtime: services}).LiveRuntimeTree())
		require.False(t, (&App{runtime: treeLivePerfServices{}}).LiveRuntimeTree())
	})

	t.Run("unsupported runtime", func(t *testing.T) {
		calls := &atomic.Int32{}
		handle := treeLivePerfHandle{metadataCalls: calls}
		require.False(t, (&App{runtime: &mockRuntime{}, currentState: sessionState{handle: handle}}).LiveRuntimeTree())
		require.EqualValues(t, 1, calls.Load())
	})
}

func BenchmarkSessionMetadataWithLongHistory(b *testing.B) {
	for _, historyMessages := range []int{100, 1000} {
		b.Run(fmt.Sprintf("history=%d", historyMessages), func(b *testing.B) {
			app := newLiveTreePerfApp(b, historyMessages, 1000)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = app.SessionHandle().Metadata()
			}
		})
	}
}

func BenchmarkAppSubagentNodeForSessionWithLongHistory(b *testing.B) {
	for _, historyMessages := range []int{0, 100, 1000} {
		for _, treeNodes := range []int{100, 1000, 5000} {
			b.Run(fmt.Sprintf("history=%d/tree=%d", historyMessages, treeNodes), func(b *testing.B) {
				app := newLiveTreePerfApp(b, historyMessages, treeNodes)
				targetSessionID := "perf-child-" + app.Session().ID
				targetNodeID, ok := app.runtime.(interface {
					SubagentNodeForSession(string) (subagent.NodeID, bool)
				}).SubagentNodeForSession(targetSessionID)
				if !ok {
					b.Fatal("fixture child has no runtime tree index")
				}
				for _, tc := range []struct {
					name   string
					lookup func(*App, string) (subagent.NodeID, bool)
				}{
					{name: "metadata-check-baseline", lookup: legacySubagentNodeForSession},
					{name: "application-index-fast-path", lookup: (*App).SubagentNodeForSession},
				} {
					b.Run(tc.name, func(b *testing.B) {
						b.ReportAllocs()
						b.ResetTimer()
						for range b.N {
							node, found := tc.lookup(app, targetSessionID)
							if !found || node != targetNodeID {
								b.Fatalf("lookup = (%q, %t), want (%q, true)", node, found, targetNodeID)
							}
						}
					})
				}
			})
		}
	}
}
