package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// Exercise production open policy with independent pools and distinct session
// owners. This does not establish a cross-process same-session ownership lease.
func TestNew_ConcurrentSessionWriters(t *testing.T) {
	for _, sandbox := range []string{"", "synthetic-vm"} {
		t.Run("sandbox="+sandbox, func(t *testing.T) {
			t.Setenv("SANDBOX_VM_ID", sandbox)
			path := filepath.Join(t.TempDir(), "synthetic.db")
			a, err := New(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, a.Close()) })
			b, err := New(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			stores := []session.Store{a, b}
			for i, store := range stores {
				require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID(fmt.Sprintf("owner-%d", i)))))
			}
			const count = 40
			start := make(chan struct{})
			results := make(chan error, len(stores))
			var wg sync.WaitGroup
			for i, store := range stores {
				wg.Go(func() {
					<-start
					id := fmt.Sprintf("owner-%d", i)
					for n := range count {
						item := session.Item{Message: session.UserMessage(fmt.Sprintf("message-%d", n))}
						writeID := fmt.Sprintf("write-%d", n)
						appender := store.(session.ItemAppender)
						original, err := appender.AppendItem(t.Context(), id, writeID, item)
						if err != nil {
							results <- err
							return
						}
						retried, err := appender.AppendItem(t.Context(), id, writeID, item)
						if err != nil || original != retried {
							results <- fmt.Errorf("append receipt %d != %d: %w", original, retried, err)
							return
						}
						if err := store.UpdateSessionTitle(t.Context(), id, fmt.Sprintf("title-%d", n)); err != nil {
							results <- err
							return
						}
					}
					results <- nil
				})
			}
			close(start)
			wg.Wait()
			for range stores {
				require.NoError(t, <-results)
			}
			for i := range stores {
				loaded, err := b.GetSession(t.Context(), fmt.Sprintf("owner-%d", i))
				require.NoError(t, err)
				require.Len(t, loaded.Messages, count)
				require.Equal(t, "title-39", loaded.Title)
				for n, item := range loaded.Messages {
					require.Equal(t, fmt.Sprintf("message-%d", n), item.Message.Message.Content)
				}
			}
		})
	}
}

func TestNew_ConcurrentMigrationPreservesSentinel(t *testing.T) {
	for _, sandbox := range []string{"", "synthetic-vm"} {
		t.Run("sandbox="+sandbox, func(t *testing.T) {
			t.Setenv("SANDBOX_VM_ID", sandbox)
			path := filepath.Join(t.TempDir(), "synthetic.db")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT); INSERT INTO sessions VALUES ('sentinel', '[]', '2025-01-01T00:00:00Z'); CREATE TABLE sentinel_schema (value TEXT); INSERT INTO sentinel_schema VALUES ('untouched')`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			type result struct {
				store session.Store
				err   error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			for range 2 {
				go func() { <-start; store, err := New(t.Context(), path); results <- result{store, err} }()
			}
			close(start)
			for range 2 {
				result := <-results
				require.NoError(t, result.err)
				_, err := result.store.GetSession(t.Context(), "sentinel")
				require.NoError(t, err)
				require.NoError(t, result.store.Close())
			}
			db, err = sql.Open("sqlite", path)
			require.NoError(t, err)
			defer db.Close()
			var value string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT value FROM sentinel_schema`).Scan(&value))
			require.Equal(t, "untouched", value)
			backups, err := filepath.Glob(path + ".bak*")
			require.NoError(t, err)
			require.Empty(t, backups)
		})
	}
}

func TestNew_LockedCanceledStartupPreservesSentinel(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "synthetic-vm")
	path := filepath.Join(t.TempDir(), "synthetic.db")
	store, err := New(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("sentinel"))))
	require.NoError(t, store.Close())
	holder, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer holder.Close()
	conn, err := holder.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`)
	require.NoError(t, err)
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(t.Context()), `ROLLBACK`) }()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = New(ctx, path)
	elapsed := time.Since(started)
	t.Logf("startup deadline50ms elapsed=%s err=%v", elapsed, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 500*time.Millisecond)
	_, err = conn.ExecContext(t.Context(), `ROLLBACK`)
	require.NoError(t, err)
	store, err = New(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	_, err = store.GetSession(t.Context(), "sentinel")
	require.NoError(t, err)
	backups, err := filepath.Glob(path + ".bak*")
	require.NoError(t, err)
	require.Empty(t, backups)
}

func TestNew_LegacyConversionFailureNeverResets(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%t", malformed), func(t *testing.T) {
			t.Setenv("SANDBOX_VM_ID", "")
			path := filepath.Join(t.TempDir(), "synthetic.db")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			defer db.Close()
			messages := `[{"Message":{"Message":{"role":"user","content":"first"}}},{"Message":{"Message":{"role":"user","content":"second"}}}]`
			if malformed {
				messages = `not valid JSON`
			}
			_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT); CREATE TABLE sentinel_schema (value TEXT); INSERT INTO sentinel_schema VALUES ('untouched')`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO sessions VALUES ('sentinel', ?, '2025-01-01T00:00:00Z')`, messages)
			require.NoError(t, err)
			if !malformed {
				// Precreate the normalized table recognized by migration14, with
				// a synthetic trigger that fails the second converted message.
				_, err = db.ExecContext(t.Context(), `CREATE TABLE session_items (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, position INTEGER, item_type TEXT, agent_name TEXT, message_json TEXT, implicit BOOLEAN, subsession_id TEXT, summary_text TEXT); CREATE TRIGGER fail_second BEFORE INSERT ON session_items WHEN NEW.position = 1 BEGIN SELECT RAISE(ABORT, 'synthetic mid-conversion failure'); END`)
				require.NoError(t, err)
			}
			_, err = New(t.Context(), path)
			require.ErrorIs(t, err, session.ErrMigrationFailed)
			var source, sentinel string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT messages FROM sessions WHERE id = 'sentinel'`).Scan(&source))
			require.Equal(t, messages, source)
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT value FROM sentinel_schema`).Scan(&sentinel))
			require.Equal(t, "untouched", sentinel)
			var items, receipts int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items`).Scan(&items))
			require.Zero(t, items)
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 15`).Scan(&receipts))
			require.Zero(t, receipts)
			backups, err := filepath.Glob(path + ".bak*")
			require.NoError(t, err)
			require.Empty(t, backups)
			if !malformed {
				_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_second`)
				require.NoError(t, err)
				store, err := New(t.Context(), path)
				require.NoError(t, err)
				defer store.Close()
				loaded, err := store.GetSession(t.Context(), "sentinel")
				require.NoError(t, err)
				require.Len(t, loaded.Messages, 2)
			}
		})
	}
}
