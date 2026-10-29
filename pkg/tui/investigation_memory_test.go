package tui

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func investigationEnabled(tb testing.TB) {
	tb.Helper()
	if os.Getenv("DA_INVESTIGATION_MEMORY") == "" {
		tb.Skip("set DA_INVESTIGATION_MEMORY=1 to run the investigation")
	}
}

func investigationEnvInt(name string, def, limit int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v <= 0 {
		return def
	}
	return min(v, limit)
}

func investigationHeapAfterGC() uint64 {
	goruntime.GC()
	goruntime.GC()
	var stats goruntime.MemStats
	goruntime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func investigationGoroutinesSettled(want int, deadline time.Duration) int {
	end := time.Now().Add(deadline)
	for {
		n := goruntime.NumGoroutine()
		if n <= want || time.Now().After(end) {
			return n
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // A checker goroutine would skew the census.
	}
}

// A nil program stalls routing without mutating a real session.
func investigationSlowSession(tb testing.TB, ctx context.Context, id string) (*supervisor.Supervisor, *app.App, string) {
	tb.Helper()
	sess := session.New(session.WithID(id))
	a := app.New(ctx, nil, sess, agentruntime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	sup := supervisor.New(nil)
	tabID, err := sup.AddSession(ctx, a, sess, "", nil)
	require.NoError(tb, err)
	return sup, a, tabID
}

// Distinct payload allocations expose byte retention rather than shared-string overhead.
func investigationFeed(feedCtx context.Context, a *app.App, payloadBytes, total int, quiet time.Duration) (accepted int64, idle time.Duration) {
	var done atomic.Int64
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	go func() {
		defer close(stop)
		for range total {
			payload := strings.Repeat("x", payloadBytes)
			a.SendPresentationEvent(feedCtx, agentruntime.ShellOutput(payload))
			done.Add(1)
			last.Store(time.Now().UnixNano())
		}
	}()
	for {
		select {
		case <-stop:
			return done.Load(), 0
		case <-time.After(10 * time.Millisecond):
		}
		if since := time.Since(time.Unix(0, last.Load())); since >= quiet {
			return done.Load(), since
		}
	}
}

func TestInvestigationFanoutSlowConsumerBacklog(t *testing.T) {
	investigationEnabled(t)
	payloadBytes := investigationEnvInt("DA_INVESTIGATION_PAYLOAD_BYTES", 4096, 1<<20)
	const total = 4000

	baseGoroutines := goruntime.NumGoroutine()
	baseHeap := investigationHeapAfterGC()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()

	sup, a, tabID := investigationSlowSession(t, ctx, "investigation-fanout")
	accepted, idle := investigationFeed(feedCtx, a, payloadBytes, total, 300*time.Millisecond)
	heldHeap := investigationHeapAfterGC()
	goruntime.KeepAlive(a)
	t.Logf("payload_bytes=%d submitted=%d accepted_before_stall=%d stalled_for=%s goroutines_running=%d",
		payloadBytes, total, accepted, idle, goruntime.NumGoroutine())
	held := int64(heldHeap) - int64(baseHeap)
	t.Logf("live_heap_held_delta_bytes=%d per_accepted_event=%d", held, held/max(accepted, 1))
	require.Less(t, accepted, int64(total), "bounded queues must backpressure a stalled consumer")

	sup.CloseSession(tabID)
	stopFeed()
	sup.Shutdown()
	cancel()
	goroutines := investigationGoroutinesSettled(baseGoroutines+2, 5*time.Second)
	afterHeap := investigationHeapAfterGC()
	t.Logf("after_close goroutines=%d base_goroutines=%d after_close_heap_delta_bytes=%d", goroutines, baseGoroutines, int64(afterHeap)-int64(baseHeap))
	require.LessOrEqual(t, goroutines, baseGoroutines+2, "closed session must release routing goroutines")
}

func TestInvestigationSessionOpenCloseCycles(t *testing.T) {
	investigationEnabled(t)
	cycles := investigationEnvInt("DA_INVESTIGATION_CYCLES", 5, 20)

	baseGoroutines := goruntime.NumGoroutine()
	baseHeap := investigationHeapAfterGC()
	for c := range cycles {
		ctx, cancel := context.WithCancel(t.Context())
		feedCtx, stopFeed := context.WithCancel(ctx)
		sup, a, tabID := investigationSlowSession(t, ctx, fmt.Sprintf("investigation-cycle-%d", c))
		accepted, _ := investigationFeed(feedCtx, a, 4096, 300, 200*time.Millisecond)
		sup.CloseSession(tabID)
		stopFeed()
		sup.Shutdown()
		cancel()
		goroutines := investigationGoroutinesSettled(baseGoroutines+2, 5*time.Second)
		t.Logf("cycle=%d accepted=%d goroutines=%d after_close_heap_delta_bytes=%d", c, accepted, goroutines, int64(investigationHeapAfterGC())-int64(baseHeap))
		require.LessOrEqual(t, goroutines, baseGoroutines+2, "cycle %d leaked routing goroutines", c)
	}
}

// Empty-root census only; zero registrations cannot validate animation release.
func TestInvestigationRootAnimationOwnershipAfterCleanup(t *testing.T) {
	investigationEnabled(t)
	ar := animation.NewRuntime()
	m, _, _ := harnessRoot(t, 120, 40, ar)
	_ = m.View()
	beforeCleanup := ar.ActiveCount()
	m.cleanupManagedResources()
	t.Logf("animation_active before_cleanup=%d after_cleanup=%d goroutines=%d", beforeCleanup, ar.ActiveCount(), goruntime.NumGoroutine())
}

// Bypass View's cache to measure composition rather than a cache hit.
func BenchmarkInvestigationRootComposeView(b *testing.B) {
	investigationEnabled(b)
	ar := animation.NewRuntime()
	m, _, _ := harnessRoot(b, 120, 40, ar)
	b.ReportAllocs()
	for b.Loop() {
		_ = m.composeView()
	}
}
