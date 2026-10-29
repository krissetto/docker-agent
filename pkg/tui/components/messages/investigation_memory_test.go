package messages

import (
	"os"
	goruntime "runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func investigationGate(tb testing.TB) {
	tb.Helper()
	if os.Getenv("DA_INVESTIGATION_MEMORY") == "" {
		tb.Skip("set DA_INVESTIGATION_MEMORY=1 to run the investigation")
	}
}

func investigationRecords() int {
	v, err := strconv.Atoi(os.Getenv("DA_INVESTIGATION_MESSAGES"))
	if err != nil || v <= 0 {
		return 100
	}
	return min(v, 5000)
}

func investigationCompact(tb testing.TB) bool {
	tb.Helper()
	switch m := os.Getenv("DA_INVESTIGATION_MODE"); m {
	case "", "plain":
		return false
	case "compact":
		return true
	default:
		tb.Fatalf("DA_INVESTIGATION_MODE must be compact or plain, got %q", m)
		return false
	}
}

func investigationNewView(tb testing.TB, ar *animation.Runtime, compact bool) *model {
	tb.Helper()
	m := NewScrollableView(ar, 120, 40, &service.SessionState{}).(*model)
	if compact {
		_ = m.SetCompactWorkLog(true)
	}
	return m
}

// Keep the source reachable across samples to exclude it from view-retention deltas.
func TestInvestigationReplayScaleAndCleanup(t *testing.T) {
	investigationGate(t)
	compact := investigationCompact(t)
	records := investigationRecords()
	sess := retainedMemoryWorkload(records)

	before := retainedHeapAfterGC()
	var pre goruntime.MemStats
	goruntime.ReadMemStats(&pre)

	ar := animation.NewRuntime()
	m := investigationNewView(t, ar, compact)
	start := time.Now()
	BeginReplay(m, PrepareReplay(sess), nil)
	drainRetainedReplay(t, m)
	finishHistoryReplay(t, m)
	wall := time.Since(start)

	var post goruntime.MemStats
	goruntime.ReadMemStats(&post)
	after := retainedHeapAfterGC()
	if path := os.Getenv("DA_INVESTIGATION_HEAP_PROFILE"); path != "" {
		// Both source and view must remain reachable for live allocation attribution.
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, pprof.WriteHeapProfile(f))
		require.NoError(t, f.Close())
		t.Logf("live_heap_profile=%s source_included=true view_alive=true", path)
	}
	goruntime.KeepAlive(m)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("records=%d session_items=%d compact=%v views=%d", records, len(sess.Messages), compact, len(m.views))
	t.Logf("replay_wall=%s total_alloc_delta_bytes=%d mallocs_delta=%d num_gc_delta=%d pause_total_delta_ns=%d",
		wall, post.TotalAlloc-pre.TotalAlloc, post.Mallocs-pre.Mallocs, post.NumGC-pre.NumGC, post.PauseTotalNs-pre.PauseTotalNs)
	t.Logf("live_heap_retained_bytes=%d (%.2f MiB) per_item=%.1f rss_kb=%d",
		retained, mib(retained), float64(retained)/float64(len(sess.Messages)), residentKB(t.Context()))

	active := ar.ActiveCount()
	m.StopAnimations()
	t.Logf("animation_active_before_stop=%d after_stop=%d", active, ar.ActiveCount())
	require.Zero(t, ar.ActiveCount(), "StopAnimations must release every registration")

	m = nil
	dropped := retainedHeapAfterGC()
	goruntime.KeepAlive(m)
	t.Logf("after_drop_heap_delta_bytes=%d goroutines=%d", int64(dropped.HeapAlloc)-int64(before.HeapAlloc), goruntime.NumGoroutine())
	goruntime.KeepAlive(sess)
}

func TestInvestigationRepeatedReplayCleanup(t *testing.T) {
	investigationGate(t)
	compact := investigationCompact(t)
	sess := retainedMemoryWorkload(investigationRecords())
	baseGoroutines := goruntime.NumGoroutine()
	baseHeap := retainedHeapAfterGC()
	for cycle := range 3 {
		ar := animation.NewRuntime()
		m := investigationNewView(t, ar, compact)
		BeginReplay(m, PrepareReplay(sess), nil)
		drainRetainedReplay(t, m)
		finishHistoryReplay(t, m)
		goruntime.KeepAlive(m)
		m.StopAnimations()
		require.Zero(t, ar.ActiveCount(), "cycle %d: animation registrations leaked", cycle)
		m = nil
		dropped := retainedHeapAfterGC()
		goruntime.KeepAlive(m)
		t.Logf("cycle=%d after_drop_heap_delta_bytes=%d goroutines=%d", cycle, int64(dropped.HeapAlloc)-int64(baseHeap.HeapAlloc), goruntime.NumGoroutine())
	}
	require.LessOrEqual(t, goruntime.NumGoroutine(), baseGoroutines+2, "replay cycles leaked goroutines")
	goruntime.KeepAlive(sess)
}

// Exclude animation cleanup from replay cost.
func BenchmarkInvestigationReplay(b *testing.B) {
	investigationGate(b)
	compact := investigationCompact(b)
	sess := retainedMemoryWorkload(investigationRecords())
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		ar := animation.NewRuntime()
		m := investigationNewView(b, ar, compact)
		prepared := PrepareReplay(sess)
		b.StartTimer()
		BeginReplay(m, prepared, nil)
		drainRetainedReplay(b, m)
		finishHistoryReplay(b, m)
		b.StopTimer()
		m.StopAnimations()
	}
}

// Settled component floor, not root-frame composition cost.
func BenchmarkInvestigationMessagesView(b *testing.B) {
	investigationGate(b)
	compact := investigationCompact(b)
	ar := animation.NewRuntime()
	m := investigationNewView(b, ar, compact)
	BeginReplay(m, PrepareReplay(retainedMemoryWorkload(investigationRecords())), nil)
	drainRetainedReplay(b, m)
	finishHistoryReplay(b, m)
	b.Cleanup(m.StopAnimations)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		m.scrollview.SetScrollOffset(i % max(1, m.totalHeight))
		_ = m.View()
	}
}

// Fixed bursts keep work per operation independent of benchmark calibration.
func BenchmarkInvestigationStreamAndTools(b *testing.B) {
	investigationGate(b)
	compact := investigationCompact(b)
	const chunks = 256
	const chunk = "Streaming a sentence of markdown with `code` and **emphasis** for the transcript. "
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		ar := animation.NewRuntime()
		m := investigationNewView(b, ar, compact)
		_ = m.AddAssistantMessage("worker", "")
		b.StartTimer()
		for i := range chunks {
			if i%16 == 15 {
				call := tools.ToolCall{ID: "call-" + strconv.Itoa(i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
				_ = m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
				_ = m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "ok"}, types.ToolStatusCompleted)
				_ = m.AddAssistantMessage("worker", "")
			}
			_ = m.AppendToLastMessage("worker", chunk)
			_ = m.View()
		}
		b.StopTimer()
		m.StopAnimations()
	}
}
