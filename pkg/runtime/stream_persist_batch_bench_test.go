package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// runBatchStreamBench queues the whole stream in the observe channel first, so
// backlog batching applies once implemented; on base it degrades to per-event
// commits. Same fixture, measurements and output keys as runStreamBench.
func runBatchStreamBench(t *testing.T, name, synchronous string) {
	t.Helper()
	ctx := t.Context()
	f := newStreamBenchFixture(t, name)
	f.quiesce(t, synchronous)
	t.Logf("BENCH variant=%s synchronous=%s", name, synchronous)
	measureLoads(t, f, "closed_stream")

	events, settle := streamBenchEvents(1, f.sess.ID)
	persistence := newPersistenceObserver(f.store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	inner := make(chan Event, len(events)+1)
	for _, event := range events {
		inner <- event
	}
	inner <- settle
	close(inner)
	before := walFrames(t, f.path)

	start := time.Now()
	for range r.observe(ctx, f.sess, inner) {
	}
	t.Logf("BENCH batch_total_time=%s events=%d", time.Since(start), len(events)+1)
	require.NoError(t, persistence.completionError(f.sess.ID))
	after := walFrames(t, f.path)
	t.Logf("BENCH wal_bytes=%d wal_frames=%d commits=%d (setup excluded: before stream wal_bytes=%d commits=%d)",
		after.bytes, after.frames, after.commits, before.bytes, before.commits)
	measureLoads(t, f, "after_settle")
	loaded, err := f.store.GetSession(ctx, f.sess.ID)
	require.NoError(t, err)
	rows := benchAssistantRows(loaded)
	require.NotEmpty(t, rows)
	t.Logf("BENCH canonical_rows_after_settle=%d", len(rows))
}

// TestStreamPersistBatchBench is the group-commit variant of TestStreamPersistBench.
// Run with STREAM_CHUNK_BENCH=1.
func TestStreamPersistBatchBench(t *testing.T) {
	requireStreamBench(t)
	db, err := sqliteutil.OpenDBWithImmediateTransactions(t.Context(), filepath.Join(t.TempDir(), "production-dsn.db"))
	require.NoError(t, err)
	require.NoError(t, sqliteutil.CloseDB(db))
	for _, sync := range []string{"FULL", "NORMAL"} {
		runBatchStreamBench(t, "batch-"+strings.ToLower(sync), sync)
	}
}
