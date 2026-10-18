package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"modernc.org/sqlite"
)

func sessionCatalogMetricsMigration(t *testing.T) Migration {
	t.Helper()
	for _, migration := range getAllMigrations() {
		if migration.ID == 40 {
			return migration
		}
	}
	t.Fatal("missing session catalog metrics migration")
	return Migration{}
}

func migrationsBeforeSessionCatalogMetrics() []Migration {
	var out []Migration
	for _, migration := range getAllMigrations() {
		if migration.ID < 40 {
			out = append(out, migration)
		}
	}
	return out
}

func TestSessionCatalogSummariesAggregatePersistedTreeMetrics(t *testing.T) {
	for name, create := range map[string]func() Store{
		"memory": NewInMemorySessionStore,
		"sqlite": func() Store { return openMemoryStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := create()
			root := New(WithID("root"), WithTitle("catalog root"), WithWorkingDir("/workspace"), WithAttributes(map[string]string{"docker-agent.actor.agent": "root", "docker-agent.actor.source": "kit.yaml"}), WithUserMessage("root question"))
			root.CreatedAt = time.Unix(200, 0).UTC()
			root.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "root answer", Cost: .25}))
			root.AddMessage(SystemMessage("not counted"))
			root.Messages = append(root.Messages, Item{Summary: "compacted", Cost: .05})
			child := New(WithID("child"), WithParentID(root.ID), WithAttributes(map[string]string{"docker-agent.actor.agent": "worker"}), WithUserMessage("child question"))
			child.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "child answer", Cost: .5}))
			grandchild := New(WithID("grandchild"), WithParentID(child.ID), WithAttributes(map[string]string{"docker-agent.actor.agent": "worker"}))
			grandchild.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "grandchild answer", Cost: 1}))
			child.AddSubSession(grandchild)
			root.AddSubSession(child)
			root.SetAttribute("docker-agent.actor.source", "kit.yaml")
			root.AgentModelOverrides = map[string]string{"root": "provider/model"}
			require.NoError(t, store.AddSession(t.Context(), root))
			rows, err := store.(SessionCatalogSummaryStore).GetSessionCatalogSummaries(t.Context())
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, root.ID, rows[0].ID)
			assert.Equal(t, root.Title, rows[0].Title)
			assert.Equal(t, root.CreatedAt, rows[0].CreatedAt)
			assert.Equal(t, root.WorkingDir, rows[0].WorkingDir)
			assert.Equal(t, "kit.yaml", rows[0].Attributes["docker-agent.actor.source"])
			assert.Equal(t, "provider/model", rows[0].AgentModelOverrides["root"])
			assert.Equal(t, 5, rows[0].NumMessages)
			assert.InDelta(t, 1.8, rows[0].Cost, 1e-9)
		})
	}
}

var catalogQueryCount atomic.Int32
var catalogLastQuery atomic.Value
var registerCatalogCountingDriver sync.Once

type catalogCountingDriver struct{}

func (catalogCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &catalogCountingConn{Conn: conn}, nil
}

type catalogCountingConn struct{ driver.Conn }

func (c *catalogCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	catalogQueryCount.Add(1)
	catalogLastQuery.Store(query)
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}

func TestSQLiteSessionCatalogSummaryUsesOneQuery(t *testing.T) {
	const driverName = "session_catalog_query_count"
	registerCatalogCountingDriver.Do(func() { sql.Register(driverName, catalogCountingDriver{}) })
	db, err := sql.Open(driverName, ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for rootIndex := range 3 {
		root := New(WithID(fmt.Sprintf("root-%d", rootIndex)), WithAttributes(map[string]string{"docker-agent.actor.agent": "root"}))
		root.AddMessage(UserMessage("root catalog row"))
		for childIndex := range 100 {
			child := New(WithID(fmt.Sprintf("root-%d-child-%03d", rootIndex, childIndex)), WithParentID(root.ID))
			for messageIndex := range 12 {
				child.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleUser, Content: fmt.Sprintf("root %d child %d message %d deterministic history", rootIndex, childIndex, messageIndex)}))
			}
			root.AddSubSession(child)
		}
		require.NoError(t, store.AddSession(t.Context(), root))
	}
	largeBody := strings.Repeat("large deterministic session transcript ", 512)
	_, err = store.db.ExecContext(t.Context(), `UPDATE session_items SET message_json = json_set(message_json, '$.content', ?) WHERE item_type = 'message'`, largeBody)
	require.NoError(t, err)
	catalogQueryCount.Store(0)
	rows, err := store.GetSessionCatalogSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, 1201, rows[0].NumMessages)
	assert.EqualValues(t, 1, catalogQueryCount.Load())
	catalogQueryCount.Store(0)
	page, err := store.GetSessionSummaryPage(t.Context(), SummaryPageOptions{Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Summaries, 3)
	assert.Equal(t, 1, page.Summaries[0].NumMessages, "paged remote catalog keeps per-session count semantics")
	assert.EqualValues(t, 1, catalogQueryCount.Load(), "remote/session-v2 paging uses a single summary-row query")
	pageQuery, _ := catalogLastQuery.Load().(string)
	assert.NotEmpty(t, pageQuery)
	assert.NotContains(t, strings.ToLower(pageQuery), "session_items", "paged remote/TUI catalog must not count history rows")
	pagePlanRows, err := store.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+pageQuery, 21)
	require.NoError(t, err)
	var pagePlan strings.Builder
	for pagePlanRows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, pagePlanRows.Scan(&id, &parent, &notused, &detail))
		pagePlan.WriteString(detail)
		pagePlan.WriteByte('\n')
	}
	require.NoError(t, pagePlanRows.Err())
	require.NoError(t, pagePlanRows.Close())
	assert.NotContains(t, pagePlan.String(), "session_items")
	assert.NotContains(t, strings.ToLower(sessionCatalogSummaryQuery), "json_extract")
	assert.NotContains(t, strings.ToLower(sessionCatalogSummaryQuery), "session_items")
	planRows, err := store.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+sessionCatalogSummaryQuery)
	require.NoError(t, err)
	defer planRows.Close()
	var plan strings.Builder
	for planRows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, planRows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	require.NoError(t, planRows.Err())
	assert.NotContains(t, plan.String(), "session_items")
	assert.NotContains(t, plan.String(), "message_json")
}

func TestSQLiteSessionCatalogMetricsFollowItemMutations(t *testing.T) {
	store := openMemoryStore(t)
	ctx := t.Context()
	root := New(WithID("metrics-root"))
	require.NoError(t, store.AddSession(ctx, root))
	child := New(WithID("metrics-child"), WithParentID(root.ID))
	require.NoError(t, store.AddSubSession(ctx, root.ID, child))
	userID, err := store.AddMessage(ctx, root.ID, UserMessage("question"))
	require.NoError(t, err)
	_, err = store.AddMessage(ctx, root.ID, SystemMessage("system message is excluded from local transcript totals"))
	require.NoError(t, err)
	assistant := NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", Cost: .25})
	assistantID, err := store.AddMessage(ctx, root.ID, assistant)
	require.NoError(t, err)
	require.NoError(t, store.AddSummary(ctx, root.ID, Item{Summary: "compacted", Cost: .05}))
	childMessage := NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "child answer", Cost: .5})
	childMessageID, err := store.AddMessage(ctx, child.ID, childMessage)
	require.NoError(t, err)
	remoteRootCount, remoteChildCount := 3, 1
	includeRemoteChild := true
	assertCatalogMetrics := func(count int, cost float64, message ...string) {
		t.Helper()
		rows, err := store.GetSessionCatalogSummaries(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, count, rows[0].NumMessages, message)
		assert.InDelta(t, cost, rows[0].Cost, 1e-9, message)
		page, err := store.GetSessionSummaryPage(ctx, SummaryPageOptions{IncludeChildren: true, Limit: 10})
		require.NoError(t, err)
		byID := make(map[string]Summary, len(page.Summaries))
		for _, row := range page.Summaries {
			byID[row.ID] = row
		}
		require.Contains(t, byID, root.ID)
		assert.Equal(t, remoteRootCount, byID[root.ID].NumMessages, "paged remote count includes all message rows, including system")
		if includeRemoteChild {
			require.Contains(t, byID, child.ID)
			assert.Equal(t, remoteChildCount, byID[child.ID].NumMessages, "paged remote count is scoped to the child row")
		} else {
			assert.NotContains(t, byID, child.ID)
		}
	}
	assertCatalogMetrics(3, .8)
	page, err := store.GetSessionSummaryPage(ctx, SummaryPageOptions{IncludeChildren: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Summaries, 2)
	pageCounts := map[string]int{}
	for _, summary := range page.Summaries {
		pageCounts[summary.ID] = summary.NumMessages
	}
	assert.Equal(t, 3, pageCounts[root.ID], "remote root row count includes system messages")
	assert.Equal(t, 1, pageCounts[child.ID], "remote child row count includes its own message")

	assistant.Message.Role = chat.MessageRoleSystem
	assistant.Message.Cost = .4
	require.NoError(t, store.UpdateMessage(ctx, root.ID, assistantID, assistant))
	assertCatalogMetrics(2, .95)
	require.NoError(t, store.AddSummary(ctx, child.ID, Item{Summary: "child compacted", Cost: .15}))
	assertCatalogMetrics(2, 1.1)

	remoteChildCount = 0
	_, err = store.db.ExecContext(ctx, `UPDATE session_items SET item_type = 'error' WHERE session_id = ? AND id = ?`, child.ID, childMessageID)
	require.NoError(t, err)
	assertCatalogMetrics(1, .6)
	remoteChildCount = 1
	_, err = store.db.ExecContext(ctx, `UPDATE session_items SET item_type = 'message' WHERE session_id = ? AND id = ?`, child.ID, childMessageID)
	require.NoError(t, err)
	assertCatalogMetrics(2, 1.1)
	remoteRootCount, remoteChildCount = 4, 0
	_, err = store.db.ExecContext(ctx, `UPDATE session_items SET session_id = ? WHERE session_id = ? AND id = ?`, root.ID, child.ID, childMessageID)
	require.NoError(t, err)
	// parent_id remains the source of membership; moving the item row changes its owner.
	assertCatalogMetrics(2, 1.1)
	remoteRootCount = 3
	_, err = store.db.ExecContext(ctx, `DELETE FROM session_items WHERE session_id = ? AND id = ?`, root.ID, childMessageID)
	require.NoError(t, err)
	assertCatalogMetrics(1, .6)
	remoteRootCount = 2
	_, err = store.db.ExecContext(ctx, `DELETE FROM session_items WHERE session_id = ? AND id = ?`, root.ID, userID)
	require.NoError(t, err)
	assertCatalogMetrics(0, .6)

	snapshot, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSession(ctx, snapshot))
	assertCatalogMetrics(0, .6, "UPDATE sessions preserves counters across metadata upsert")
	require.NoError(t, store.DeleteSession(ctx, child.ID))
	includeRemoteChild, remoteRootCount = false, 2
	page, err = store.GetSessionSummaryPage(ctx, SummaryPageOptions{IncludeChildren: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Summaries, 1)
	assert.Equal(t, 2, page.Summaries[0].NumMessages, "child deletion preserves the surviving root page count")
	assertCatalogMetrics(0, .45, "deleting a child removes it from root ancestry totals")
	require.NoError(t, store.DeleteSession(ctx, root.ID))
	rows, err := store.GetSessionCatalogSummaries(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "root delete removes the catalog row")
}

func TestSessionCatalogMetricsMigrationBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	migrations := getAllMigrations()
	metricsMigration := sessionCatalogMetricsMigration(t)
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrationsBeforeSessionCatalogMetrics()).InitializeMigrations(t.Context()))
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, title, created_at, attributes) VALUES ('root', 'legacy root', '2026-01-01T00:00:00Z', '{"docker-agent.actor.agent":"root"}'), ('child', 'legacy child', '2026-01-02T00:00:00Z', '{}')`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE sessions SET parent_id = 'root' WHERE id = 'child'`)
	require.NoError(t, err)
	const history = 3600
	body := strings.Repeat("deterministic legacy history payload.", 24)
	for i := range history {
		message, marshalErr := json.Marshal(chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("%d:%s", i, body), Cost: .125})
		require.NoError(t, marshalErr)
		_, err = db.ExecContext(t.Context(), `INSERT INTO session_items (session_id, position, item_type, message_json) VALUES ('child', ?, 'message', ?)`, i, string(message))
		require.NoError(t, err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO session_items (session_id, position, item_type, summary_text, cost) VALUES ('root', 0, 'summary', 'compacted', .5)`)
	require.NoError(t, err)
	started := time.Now()
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrations).applyMigration(t.Context(), &metricsMigration))
	elapsed := time.Since(started)
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	rows, err := store.GetSessionCatalogSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, history, rows[0].NumMessages)
	assert.InDelta(t, float64(history)*.125+.5, rows[0].Cost, 1e-9)
	t.Logf("catalog metrics migration backfill rows=%d payload_bytes=%d elapsed=%s", history, len(body)*history, elapsed)
}

func TestSQLiteSessionCatalogMetricsMigrationRejectsCorruptMessage(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	migrations := getAllMigrations()
	metricsMigration := sessionCatalogMetricsMigration(t)
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrationsBeforeSessionCatalogMetrics()).InitializeMigrations(t.Context()))
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, created_at) VALUES ('corrupt', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO session_items (session_id, position, item_type, message_json) VALUES ('corrupt', 0, 'message', 'not-json')`)
	require.NoError(t, err)
	err = NewMigrationManagerWithMigrations(db, migrations).applyMigration(t.Context(), &metricsMigration)
	require.ErrorContains(t, err, "invalid message JSON")
	var columnCount int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name IN ('catalog_num_messages', 'catalog_cost')`).Scan(&columnCount))
	assert.Zero(t, columnCount, "failed transactional backfill rolls back schema and trigger installation")
	_, err = db.ExecContext(t.Context(), `DELETE FROM session_items WHERE session_id = 'corrupt'`)
	require.NoError(t, err)
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrations).applyMigration(t.Context(), &metricsMigration))
}

func TestSessionCatalogMetricsMissingAndNullCost(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	migrations := getAllMigrations()
	metricsMigration := sessionCatalogMetricsMigration(t)
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrationsBeforeSessionCatalogMetrics()).InitializeMigrations(t.Context()))
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, created_at) VALUES ('root', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO session_items (session_id, position, item_type, message_json) VALUES
		('root', 0, 'message', '{"role":"assistant"}'),
		('root', 1, 'message', '{"role":"assistant","cost":null}')`)
	require.NoError(t, err)
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrations).applyMigration(t.Context(), &metricsMigration))
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	rows, err := store.GetSessionCatalogSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].NumMessages)
	assert.Zero(t, rows[0].Cost)
}

func TestSessionCatalogMetricsMigrationRejectsStringCost(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	migrations := getAllMigrations()
	metricsMigration := sessionCatalogMetricsMigration(t)
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrationsBeforeSessionCatalogMetrics()).InitializeMigrations(t.Context()))
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, created_at) VALUES ('root', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO session_items (session_id, position, item_type, message_json) VALUES ('root', 0, 'message', '{"role":"assistant","cost":"0.5"}')`)
	require.NoError(t, err)
	err = NewMigrationManagerWithMigrations(db, migrations).applyMigration(t.Context(), &metricsMigration)
	require.ErrorContains(t, err, "invalid message JSON")
}

func BenchmarkSQLiteSessionCatalogSummaryLargeHistory(b *testing.B) {
	store := openMemoryStoreForBenchmark(b)
	defer store.Close()
	for rootIndex := range 3 {
		root := New(WithID(fmt.Sprintf("root-%d", rootIndex)), WithAttributes(map[string]string{"docker-agent.actor.agent": "root"}))
		for childIndex := range 100 {
			child := New(WithID(fmt.Sprintf("root-%d-child-%03d", rootIndex, childIndex)), WithParentID(root.ID))
			for messageIndex := range 12 {
				child.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleUser, Content: fmt.Sprintf("root %d child %d message %d deterministic history payload %s", rootIndex, childIndex, messageIndex, strings.Repeat("x", 1024))}))
			}
			root.AddSubSession(child)
		}
		if err := store.AddSession(context.Background(), root); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rows, err := store.GetSessionCatalogSummaries(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 3 || rows[0].NumMessages != 1200 || math.IsNaN(rows[0].Cost) {
			b.Fatalf("unexpected catalog metrics: %+v", rows)
		}
	}
}

func openMemoryStoreForBenchmark(b *testing.B) *SQLiteSessionStore {
	b.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewSQLiteSessionStoreFromDB(context.Background(), db)
	if err != nil {
		b.Fatal(err)
	}
	return store
}
