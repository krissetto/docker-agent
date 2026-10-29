package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sqliteutil"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// Fresh streams share an immutable script to exclude provider/network variation.
type investigationProvider struct {
	id        string
	responses []chat.MessageStreamResponse
}

func (p *investigationProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *investigationProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &mockStream{responses: p.responses}, nil
}

func (p *investigationProvider) BaseConfig() base.Config { return base.Config{} }
func (p *investigationProvider) MaxTokens() int          { return 0 }

func investigationStreamScript(chunks int) []chat.MessageStreamResponse {
	b := newStreamBuilder()
	for i := range chunks {
		b.AddContent(fmt.Sprintf("chunk %d ", i))
	}
	return b.AddStopWithUsage(1, int64(chunks)).responses
}

func newInvestigationRuntime(tb testing.TB, prov *investigationProvider) *LocalRuntime {
	tb.Helper()
	root := agent.New("root", "investigation agent", agent.WithModel(prov))
	rt, err := NewLocalRuntime(tb.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(tb, err)
	return rt
}

// Two GCs exclude garbage and weak runtime caches from live-heap comparisons.
func investigationHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// Without Release, driver cleanup must come from Runtime.Close.
func investigationOpenCloseCycle(tb testing.TB, rt *LocalRuntime, id string, release bool) {
	tb.Helper()
	ctx := tb.Context()
	sess := session.New(session.WithID(id), session.WithAgentName("root"), session.WithUserMessage("hi"))
	handle, err := rt.CreateSession(ctx, sess, SessionBinding{AgentName: "root"})
	require.NoError(tb, err)
	if release {
		require.NoError(tb, handle.Release(ctx))
	}
}

func BenchmarkInvestigationOpenClose(b *testing.B) {
	prov := &investigationProvider{id: "test/mock-model"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		rt := newInvestigationRuntime(b, prov)
		investigationOpenCloseCycle(b, rt, fmt.Sprintf("open-close-%d", i), true)
		require.NoError(b, rt.Close())
	}
}

// Exclude runtime setup and Close to isolate per-turn streaming cost.
func BenchmarkInvestigationStreamingTurn(b *testing.B) {
	for _, chunks := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("chunks=%d", chunks), func(b *testing.B) {
			script := investigationStreamScript(chunks)
			b.ReportAllocs()
			events := 0
			for i := 0; b.Loop(); i++ {
				// Fresh runtime per turn keeps store growth out of the measurement.
				b.StopTimer()
				rt := newInvestigationRuntime(b, &investigationProvider{id: "test/mock-model", responses: script})
				b.StartTimer()
				sess := session.New(session.WithID(fmt.Sprintf("stream-%d", i)), session.WithUserMessage("stream"))
				for range rt.runExecution(b.Context(), sess) {
					events++
				}
				b.StopTimer()
				require.NoError(b, rt.Close())
				b.StartTimer()
			}
			b.ReportMetric(float64(events)/float64(b.N), "events/op")
		})
	}
}

func TestInvestigationOwnerReturnAfterCleanup(t *testing.T) {
	if os.Getenv("INVESTIGATION_LIFECYCLE") != "1" {
		t.Skip("set INVESTIGATION_LIFECYCLE=1")
	}
	const cycles = 50
	for _, release := range []bool{true, false} {
		name := "close_only"
		if release {
			name = "release_then_close"
		}
		t.Run(name, func(t *testing.T) {
			prov := &investigationProvider{id: "test/mock-model"}
			// Warm-up cycle so lazily started package goroutines are part of the baseline.
			warm := newInvestigationRuntime(t, prov)
			investigationOpenCloseCycle(t, warm, "warm", release)
			require.NoError(t, warm.Close())

			baseGoroutines := runtime.NumGoroutine()
			baseHeap := investigationHeap()
			var maxDrivers int
			for i := range cycles {
				rt := newInvestigationRuntime(t, prov)
				investigationOpenCloseCycle(t, rt, fmt.Sprintf("cycle-%d", i), release)
				maxDrivers = max(maxDrivers, len(rt.sessionDrivers.drivers))
				require.NoError(t, rt.Close())
			}

			deadline := time.Now().Add(3 * time.Second)
			afterGoroutines := runtime.NumGoroutine()
			for afterGoroutines > baseGoroutines && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond) //nolint:forbidigo // A checker goroutine would skew the census.
				afterGoroutines = runtime.NumGoroutine()
			}
			afterHeap := investigationHeap()
			t.Logf("mode=%s cycles=%d goroutines_base=%d goroutines_after=%d heap_base=%d heap_after=%d heap_delta_per_cycle=%.0f max_drivers_per_runtime=%d",
				name, cycles, baseGoroutines, afterGoroutines, baseHeap, afterHeap,
				(float64(afterHeap)-float64(baseHeap))/cycles, maxDrivers)
			if dir := os.Getenv("INVESTIGATION_OUT"); dir != "" && afterGoroutines > baseGoroutines {
				f, err := os.Create(filepath.Join(dir, "goroutines-after-"+name+".txt"))
				require.NoError(t, err)
				defer f.Close()
				require.NoError(t, pprof.Lookup("goroutine").WriteTo(f, 1))
			}
			require.LessOrEqual(t, afterGoroutines, baseGoroutines, "goroutines must return to baseline after Close")
		})
	}
}

func investigationTranscript(id, parentID string, n int) *session.Session {
	sess := session.New(session.WithID(id), session.WithParentID(parentID), session.WithAgentName("root"))
	sess.SetAttribute(SessionAgentAttribute, "root")
	reply := strings.Repeat("Checked the owner boundary and the snapshot path. ", 4)
	for i := range n {
		if i%2 == 0 {
			sess.AddMessage(session.UserMessage(fmt.Sprintf("step %d: inspect the runtime owner", i)))
			continue
		}
		sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: reply}})
	}
	return sess
}

// Use the public view path so cold restore includes ancestry hydration.
func TestInvestigationColdSQLiteRestore(t *testing.T) {
	if os.Getenv("INVESTIGATION_COLD") != "1" {
		t.Skip("set INVESTIGATION_COLD=1")
	}
	n := 5000
	if raw := os.Getenv("INVESTIGATION_COLD_MESSAGES"); raw != "" {
		_, err := fmt.Sscanf(raw, "%d", &n)
		require.NoError(t, err)
	}
	ctx := t.Context()
	db, err := sqliteutil.OpenDBWithImmediateTransactions(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store, err := session.NewSQLiteSessionStoreFromDB(ctx, db)
	require.NoError(t, err)
	rootID, childID := investigationColdFixture(t, ctx, store, n, 0)
	runtime.GC()

	for _, tc := range []struct{ name, id string }{{"root", rootID}, {"child", childID}} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &investigationProvider{id: "test/mock-model"}
			rt := newInvestigationTeamRuntime(t, prov, WithSessionStore(store))
			runtimeView := NewSessionRuntimeSupervisor(rt).Runtime()

			baseHeap := investigationHeap()
			committed, err := OpenSessionView(ctx, runtimeView, tc.id)
			require.NoError(t, err)
			withView := investigationHeap()
			require.False(t, committed.SessionHandle.Metadata().Historical, "must measure the ordinary restore path")
			messages := committed.Info.Session.AllMessageCount()
			require.NoError(t, committed.SessionHandle.Release(ctx))
			committed = CommittedSessionView{}
			require.NoError(t, rt.Close())
			afterClose := investigationHeap()
			runtime.KeepAlive(committed)
			t.Logf("case=%s messages=%d retained_view_bytes=%d bytes_per_message=%.1f residual_after_release_close=%d",
				tc.name, messages, int64(withView)-int64(baseHeap), float64(int64(withView)-int64(baseHeap))/float64(messages), int64(afterClose)-int64(baseHeap))
		})
	}
}

// Restore requires admitted child membership and the matching async-subagent grant.
func investigationColdFixture(t *testing.T, ctx context.Context, store session.Store, n, props int) (string, string) {
	t.Helper()
	root := investigationSchemaTranscript("cold-root", "", n, props)
	require.NoError(t, store.AddSession(ctx, root))
	child := investigationSchemaTranscript("cold-child", root.ID, n, props)
	child.AgentName = "worker"
	child.SetAttribute(SessionAgentAttribute, "worker")
	child.SetAttribute(SessionParentAgentAttribute, "root")
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: subagent.NodeID("cold-child-node"), Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "worker", Name: "worker"}}
	coordinator, ok := store.(session.CoordinationStore)
	require.True(t, ok, "store must admit children")
	require.NoError(t, coordinator.AdmitChild(ctx, session.ChildAdmission{Child: child, Record: record}))
	return root.ID, child.ID
}

func newInvestigationTeamRuntime(tb testing.TB, prov *investigationProvider, opts ...Opt) *LocalRuntime {
	tb.Helper()
	root := agent.New("root", "investigation agent", agent.WithModel(prov),
		agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "worker"}))
	worker := agent.New("worker", "investigation worker", agent.WithModel(prov))
	rt, err := NewLocalRuntime(tb.Context(), team.New(team.WithAgents(root, worker)),
		append([]Opt{WithSessionCompaction(false), WithModelStore(mockModelStore{})}, opts...)...)
	require.NoError(tb, err)
	return rt
}

// investigationSchemaTranscript is investigationTranscript with a shell tool
// schema of props properties on every assistant message (props 0: none).
func investigationSchemaTranscript(id, parentID string, n, props int) *session.Session {
	if props == 0 {
		return investigationTranscript(id, parentID, n)
	}
	sess := session.New(session.WithID(id), session.WithParentID(parentID), session.WithAgentName("root"))
	sess.SetAttribute(SessionAgentAttribute, "root")
	reply := strings.Repeat("Checked the owner boundary and the snapshot path. ", 4)
	for i := range n {
		if i%2 == 0 {
			sess.AddMessage(session.UserMessage(fmt.Sprintf("step %d: inspect the runtime owner", i)))
			continue
		}
		sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{
			Role:            chat.MessageRoleAssistant,
			Content:         reply,
			ToolDefinitions: []tools.Tool{{Name: "shell", Description: "Run a shell command", Parameters: investigationRuntimeSchema(props)}},
		}})
	}
	return sess
}

func investigationRuntimeSchema(props int) map[string]any {
	properties := make(map[string]any, props)
	for i := range props {
		properties[fmt.Sprintf("arg%d", i)] = map[string]any{"type": "string", "description": "argument description"}
	}
	return map[string]any{"type": "object", "required": []any{"arg0"}, "properties": properties}
}

// Drop the caller result before measuring the held private preparation.
func TestInvestigationPreparedViewRetention(t *testing.T) {
	if os.Getenv("INVESTIGATION_PREPARED") != "1" {
		t.Skip("set INVESTIGATION_PREPARED=1")
	}
	n, props := 1000, 16
	if raw := os.Getenv("INVESTIGATION_PREPARED_MESSAGES"); raw != "" {
		_, err := fmt.Sscanf(raw, "%d", &n)
		require.NoError(t, err)
	}
	if raw := os.Getenv("INVESTIGATION_PREPARED_PROPS"); raw != "" {
		_, err := fmt.Sscanf(raw, "%d", &props)
		require.NoError(t, err)
	}
	ctx := t.Context()
	db, err := sqliteutil.OpenDBWithImmediateTransactions(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store, err := session.NewSQLiteSessionStoreFromDB(ctx, db)
	require.NoError(t, err)
	_, childID := investigationColdFixture(t, ctx, store, n, props)
	rt := newInvestigationTeamRuntime(t, &investigationProvider{id: "test/mock-model"}, WithSessionStore(store))
	t.Cleanup(func() { _ = rt.Close() })
	preparer := NewSessionRuntimeSupervisor(rt).Runtime().(SessionViewPreparer)
	runtime.GC()

	baseHeap := investigationHeap()
	prepared, err := preparer.PrepareSessionView(ctx, childID)
	require.NoError(t, err)
	committed, err := prepared.Commit(ctx)
	require.NoError(t, err)
	require.NotNil(t, committed.SessionHandle)
	committed = CommittedSessionView{}
	withPrepared := investigationHeap()
	runtime.KeepAlive(committed)
	runtime.KeepAlive(prepared)
	prepared = nil
	afterDrop := investigationHeap()
	runtime.KeepAlive(prepared)
	t.Logf("fixture messages=%d props=%d prepared_retained_after_commit_bytes=%d residual_after_prepared_drop_bytes=%d",
		n, props, int64(withPrepared)-int64(baseHeap), int64(afterDrop)-int64(baseHeap))
}
