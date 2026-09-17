package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Synthetic pre-15 schema only; no object fixture or existing user database.
func openLegacyMessagesDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "synthetic.db")+"?_pragma=busy_timeout(50)&_pragma=journal_mode(WAL)&_txlock=immediate")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	var before []Migration
	for _, migration := range getAllMigrations() {
		if migration.ID < 15 {
			before = append(before, migration)
		}
	}
	require.NoError(t, NewMigrationManagerWithMigrations(db, before).InitializeMigrations(t.Context()))
	return db
}

func legacyMessagesMigration(t *testing.T) Migration {
	t.Helper()
	for _, migration := range getAllMigrations() {
		if migration.ID == 15 {
			return migration
		}
	}
	t.Fatal("missing legacy messages migration")
	return Migration{}
}

func TestLegacyMessagesMigrationAtomicFailure(t *testing.T) {
	db := openLegacyMessagesDB(t)
	messages, err := json.Marshal([]Item{{Message: UserMessage("sentinel-first")}, {Message: UserMessage("sentinel-second")}})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, messages, created_at) VALUES ('sentinel', ?, '2025-01-01T00:00:00Z')`, string(messages))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_second BEFORE INSERT ON session_items WHEN NEW.position = 1 BEGIN SELECT RAISE(ABORT, 'synthetic mid-conversion failure'); END`)
	require.NoError(t, err)
	migration := legacyMessagesMigration(t)
	manager := NewMigrationManagerWithMigrations(db, []Migration{migration})
	err = manager.applyMigration(t.Context(), &migration)
	var receipts, items int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 15`).Scan(&receipts))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items`).Scan(&items))
	t.Logf("midconversion error=%v migration15 receipts=%d items=%d", err, receipts, items)
	require.Error(t, err)
	require.Zero(t, receipts)
	require.Zero(t, items)
	var source string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT messages FROM sessions WHERE id = 'sentinel'`).Scan(&source))
	require.Equal(t, string(messages), source)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_second`)
	require.NoError(t, err)
	require.NoError(t, manager.applyMigration(t.Context(), &migration))
	require.NoError(t, manager.applyMigration(t.Context(), &migration))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items`).Scan(&items))
	require.Equal(t, 2, items)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 15`).Scan(&receipts))
	require.Equal(t, 1, receipts)
}

func TestLegacyMessagesMigrationCancellationAndWriterHold(t *testing.T) {
	db := openLegacyMessagesDB(t)
	const count = 1000
	items := make([]Item, count)
	for i := range items {
		items[i] = Item{Message: UserMessage("synthetic representative legacy input")}
	}
	messages, err := json.Marshal(items)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, messages, created_at) VALUES ('sentinel', ?, '2025-01-01T00:00:00Z')`, string(messages))
	require.NoError(t, err)
	migration := legacyMessagesMigration(t)
	require.NotNil(t, migration.UpTxFunc)
	original := migration.UpTxFunc
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	migration.UpTxFunc = func(ctx context.Context, tx *sql.Tx) error {
		if err := original(ctx, tx); err != nil {
			return err
		}
		cancel() // All converted rows exist but neither they nor the receipt committed.
		return ctx.Err()
	}
	manager := NewMigrationManagerWithMigrations(db, []Migration{migration})
	require.ErrorIs(t, manager.applyMigration(ctx, &migration), context.Canceled)
	var converted, receipts int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items`).Scan(&converted))
	require.Zero(t, converted)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 15`).Scan(&receipts))
	require.Zero(t, receipts)
	migration.UpTxFunc = original
	started := time.Now()
	require.NoError(t, manager.applyMigration(t.Context(), &migration))
	t.Logf("atomic legacy migration rows=%d source_bytes=%d writer_hold=%s", count, len(messages), time.Since(started))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items`).Scan(&converted))
	require.Equal(t, count, converted)
}

func TestLegacyMessagesMigrationConcurrentReceipt(t *testing.T) {
	db := openLegacyMessagesDB(t)
	var seq int
	var name, path string
	require.NoError(t, db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&seq, &name, &path))
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(50)&_pragma=journal_mode(WAL)&_txlock=immediate")
	require.NoError(t, err)
	other.SetMaxOpenConns(1)
	defer other.Close()
	messages, err := json.Marshal([]Item{{Message: UserMessage("sentinel")}})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions (id, messages, created_at) VALUES ('sentinel', ?, '2025-01-01T00:00:00Z')`, string(messages))
	require.NoError(t, err)
	migration := legacyMessagesMigration(t)
	original := migration.UpTxFunc
	require.NotNil(t, original)
	converted := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	migration.UpTxFunc = func(ctx context.Context, tx *sql.Tx) error {
		if err := original(ctx, tx); err != nil {
			return err
		}
		close(converted)
		<-release
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- NewMigrationManagerWithMigrations(db, nil).applyMigration(t.Context(), &migration) }()
	<-converted
	second := make(chan error, 1)
	go func() {
		second <- NewMigrationManagerWithMigrations(other, nil).applyMigration(t.Context(), &migration)
	}()
	select {
	case err := <-second:
		t.Fatalf("migration returned before conversion committed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	var items, receipts int
	require.NoError(t, other.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items`).Scan(&items))
	require.Equal(t, 1, items)
	require.NoError(t, other.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 15`).Scan(&receipts))
	require.Equal(t, 1, receipts)
}
