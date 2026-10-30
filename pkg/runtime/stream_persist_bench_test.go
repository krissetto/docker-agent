package runtime

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sqliteutil"
	"github.com/docker/docker-agent/pkg/tools"
)

// Opt-in streaming persistence benchmark. It runs only with STREAM_CHUNK_BENCH=1
// and never under -short. It uses only APIs that exist on the base code, so the
// same harness measures BEFORE (per-delta UpdateMessage) and AFTER (chunk rows).
//
//	STREAM_CHUNK_BENCH=1 go test ./pkg/runtime -run TestStreamPersistBench -v -count=1 -timeout 30m
const (
	streamBenchHistory         = 6000
	streamBenchContentDeltas   = 4000
	streamBenchReasoningDeltas = 1000
	streamBenchToolDeltas      = 100
	streamBenchToolCalls       = 5
)

type streamBenchFixture struct {
	path  string
	db    *sql.DB
	store *session.SQLiteSessionStore
	sess  *session.Session
}

// benchAssistantRows returns the assistant rows in order. It is local so this
// file compiles against the base code, which has no stream-chunk helpers.
func benchAssistantRows(sess *session.Session) []*session.Message {
	var rows []*session.Message
	for _, item := range sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			rows = append(rows, item.Message)
		}
	}
	return rows
}

func requireStreamBench(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("stream persistence benchmark is skipped under -short")
	}
	if os.Getenv("STREAM_CHUNK_BENCH") != "1" {
		t.Skip("set STREAM_CHUNK_BENCH=1 to run the stream persistence benchmark")
	}
}

// newStreamBenchFixture opens an isolated file DB (under TMPDIR) with the
// production DSN and seeds streamBenchHistory canonical messages in one
// transaction. Setup bytes and commits are excluded by the caller.
func newStreamBenchFixture(t *testing.T, name string) *streamBenchFixture {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), name+".db")
	db, err := sqliteutil.OpenDBWithImmediateTransactions(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqliteutil.CloseDB(db) })
	store, err := session.NewSQLiteSessionStoreFromDB(ctx, db)
	require.NoError(t, err)
	sess := session.New(session.WithID("stream-bench"), session.WithTitle("stream bench"))
	for i := range streamBenchHistory {
		if i%2 == 0 {
			sess.AddMessage(session.UserMessage(fmt.Sprintf("question %d: %s", i, strings.Repeat("q", 120))))
		} else {
			sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("answer %d: %s", i, strings.Repeat("a", 240))}))
		}
	}
	require.NoError(t, store.AddSession(ctx, sess))
	return &streamBenchFixture{path: path, db: db, store: store, sess: sess}
}

// quiesce makes the WAL measurable: no auto-checkpoints, and the pre-stream
// state is checkpointed away so only stream frames remain in the WAL.
func (f *streamBenchFixture) quiesce(t *testing.T, synchronous string) {
	t.Helper()
	ctx := t.Context()
	_, err := f.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint = 0")
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, "PRAGMA synchronous = "+synchronous)
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
}

type walStats struct {
	bytes   int64
	frames  int
	commits int
}

// walFrames parses the WAL file (32-byte header, then 24-byte frame headers
// followed by one page each). A frame whose db-size field is nonzero ends a
// commit. Bytes are the exact WAL file size.
func walFrames(t *testing.T, path string) walStats {
	t.Helper()
	data, err := os.ReadFile(path + "-wal")
	if os.IsNotExist(err) {
		return walStats{}
	}
	require.NoError(t, err)
	if len(data) < 32 {
		return walStats{bytes: int64(len(data))}
	}
	pageSize := int(binary.BigEndian.Uint32(data[8:12]))
	if pageSize == 1 {
		pageSize = 65536
	}
	stats := walStats{bytes: int64(len(data))}
	frameSize := 24 + pageSize
	for off := 32; off+frameSize <= len(data); off += frameSize {
		stats.frames++
		if binary.BigEndian.Uint32(data[off+4:off+8]) != 0 {
			stats.commits++
		}
	}
	return stats
}

func streamBenchEvents(seed uint64, sessionID string) ([]Event, Event) {
	rng := rand.New(rand.NewPCG(seed, 11))
	kinds := make([]byte, 0, streamBenchContentDeltas+streamBenchReasoningDeltas+streamBenchToolDeltas)
	for range streamBenchContentDeltas {
		kinds = append(kinds, 'c')
	}
	for range streamBenchReasoningDeltas {
		kinds = append(kinds, 'r')
	}
	for range streamBenchToolDeltas {
		kinds = append(kinds, 'p')
	}
	rng.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	properties := map[string]any{}
	for i := range 24 {
		properties[fmt.Sprintf("field_%02d", i)] = map[string]any{"type": "string", "description": strings.Repeat("schema text ", 6)}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	const agent = "root"
	text := func() string {
		n := 8 + rng.IntN(57)
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + rng.IntN(26))
		}
		return string(b)
	}
	var content, reasoning strings.Builder
	events := make([]Event, 0, len(kinds)+streamBenchToolCalls*2)
	partials := 0
	for _, kind := range kinds {
		switch kind {
		case 'c':
			delta := text()
			content.WriteString(delta)
			events = append(events, AgentChoice(agent, sessionID, delta))
		case 'r':
			delta := text()
			reasoning.WriteString(delta)
			events = append(events, AgentChoiceReasoning(agent, sessionID, delta))
		case 'p':
			call := partials % streamBenchToolCalls
			def := tools.Tool{Name: fmt.Sprintf("bench_tool_%d", call), Category: "builtin", Description: "bench tool", Parameters: schema}
			tc := tools.ToolCall{ID: fmt.Sprintf("call-%d", call), Type: "function", Function: tools.FunctionCall{Arguments: text()}}
			if partials < streamBenchToolCalls {
				tc.Function.Name = def.Name
			}
			events = append(events, PartialToolCall(tc, def, agent))
			partials++
		}
	}
	for call := range streamBenchToolCalls {
		def := tools.Tool{Name: fmt.Sprintf("bench_tool_%d", call), Category: "builtin", Description: "bench tool", Parameters: schema}
		tc := tools.ToolCall{ID: fmt.Sprintf("call-%d", call), Type: "function", Function: tools.FunctionCall{Name: def.Name, Arguments: "{}"}}
		events = append(events, ToolCall(tc, def, agent), ToolCallResponse(tc.ID, def, tools.ResultSuccess("ok"), "ok", agent))
	}
	settle := MessageAdded(sessionID, session.NewAgentMessage(agent, &chat.Message{Role: chat.MessageRoleAssistant, Content: content.String(), ReasoningContent: reasoning.String()}), agent)
	return events, settle
}

func percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

func medianLoad(t *testing.T, reps int, fn func() error) time.Duration {
	t.Helper()
	var durations []time.Duration
	for range reps {
		start := time.Now()
		require.NoError(t, fn())
		durations = append(durations, time.Since(start))
	}
	return percentile(durations, 0.5)
}

// measureLoads times the 6k-message readers the product uses on a session.
func measureLoads(t *testing.T, f *streamBenchFixture, label string) {
	t.Helper()
	ctx := t.Context()
	get := medianLoad(t, 5, func() error {
		_, err := f.store.GetSession(ctx, f.sess.ID)
		return err
	})
	recent := medianLoad(t, 5, func() error {
		_, err := f.store.ReadRecentChatItems(ctx, f.sess.ID, 0, 0, 20, 200, 2048)
		return err
	})
	catalog := medianLoad(t, 5, func() error {
		_, err := f.store.GetSessionCatalogSummaries(ctx)
		return err
	})
	t.Logf("BENCH load %s get_session_p50=%s recent_chat_p50=%s catalog_p50=%s", label, get, recent, catalog)
}

// runStreamBench drives one stream through the persistence observer one OnEvent
// at a time (no backlog). The batch variant lives in stream_persist_batch_bench_test.go.
func runStreamBench(t *testing.T, name, synchronous string) {
	t.Helper()
	ctx := t.Context()
	f := newStreamBenchFixture(t, name)
	f.quiesce(t, synchronous)
	t.Logf("BENCH variant=%s synchronous=%s", name, synchronous)
	measureLoads(t, f, "closed_stream")

	events, settle := streamBenchEvents(1, f.sess.ID)
	persistence := newPersistenceObserver(f.store)
	persistence.lifetime = ctx
	before := walFrames(t, f.path)

	var durations []time.Duration
	start := time.Now()
	persistence.OnEvent(ctx, f.sess, events[0])
	startLatency := time.Since(start)
	durations = append(durations, startLatency)
	for i, event := range events[1:] {
		if i == len(events)/2 {
			measureLoads(t, f, "open_stream_midway")
		}
		start = time.Now()
		persistence.OnEvent(ctx, f.sess, event)
		durations = append(durations, time.Since(start))
	}
	t.Logf("BENCH stream_start_latency=%s", startLatency)
	start = time.Now()
	persistence.OnEvent(ctx, f.sess, settle)
	t.Logf("BENCH settle_latency=%s", time.Since(start))
	require.NoError(t, persistence.completionError(f.sess.ID))
	t.Logf("BENCH per_chunk_p50=%s per_chunk_p99=%s chunks=%d", percentile(durations, 0.5), percentile(durations, 0.99), len(durations))
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

// TestStreamPersistBench measures the per-delta (OnEvent) variant. Run with
// STREAM_CHUNK_BENCH=1; each variant uses a fresh isolated DB. This file and
// stream_persist_batch_bench_test.go compile standalone on the base commit.
func TestStreamPersistBench(t *testing.T) {
	requireStreamBench(t)
	db, err := sqliteutil.OpenDBWithImmediateTransactions(t.Context(), filepath.Join(t.TempDir(), "production-dsn.db"))
	require.NoError(t, err)
	var mode string
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&mode))
	require.NoError(t, sqliteutil.CloseDB(db))
	t.Logf("BENCH production_dsn_synchronous=%s (PRAGMA synchronous on sqliteutil DSN; 2=FULL 1=NORMAL)", mode)
	for _, sync := range []string{"FULL", "NORMAL"} {
		runStreamBench(t, "per-chunk-"+strings.ToLower(sync), sync)
	}
}
