package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Every database in this file is synthetic and lives only in t.TempDir.
func openContentionDB(t *testing.T, path, mode string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1000)&_pragma=journal_mode("+mode+")")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(t.Context()))
	return db
}

// Pin the driver-level failure: a deferred reader cannot upgrade after another
// handle commits, even with a busy timeout. This remains a baseline probe, not
// the production transaction policy.
func TestSQLiteDeferredUpgradeBaseline(t *testing.T) {
	for _, mode := range []string{"WAL", "DELETE"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "synthetic.db")
			a := openContentionDB(t, path, mode)
			b := openContentionDB(t, path, mode)
			_, err := a.ExecContext(t.Context(), `CREATE TABLE probe (id INTEGER PRIMARY KEY, value TEXT); INSERT INTO probe VALUES (1, 'sentinel'), (2, 'other')`)
			require.NoError(t, err)
			reader, err := a.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = reader.Rollback() }()
			var value string
			require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT value FROM probe WHERE id = 1`).Scan(&value))
			writer, err := b.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			_, err = writer.ExecContext(t.Context(), `UPDATE probe SET value = 'committed' WHERE id = 2`)
			require.NoError(t, err)
			if mode == "WAL" {
				require.NoError(t, writer.Commit())
			}
			started := time.Now()
			_, err = reader.ExecContext(t.Context(), `UPDATE probe SET value = 'must not persist' WHERE id = 1`)
			require.Error(t, err)
			var coded interface{ Code() int }
			require.ErrorAs(t, err, &coded)
			wantCode := 5 // SQLITE_BUSY: DELETE reader/writer deadlock.
			if mode == "WAL" {
				wantCode = 517 // SQLITE_BUSY_SNAPSHOT.
			}
			require.Equal(t, wantCode, coded.Code())
			t.Logf("mode=%s upgrade code=%d elapsed=%s busy_timeout=1s", mode, coded.Code(), time.Since(started))
			require.True(t, IsTemporary(classifySQLiteError(err)))
			require.NoError(t, reader.Rollback())
			if mode == "DELETE" {
				require.NoError(t, writer.Commit())
			}
			require.NoError(t, a.QueryRowContext(t.Context(), `SELECT value FROM probe WHERE id = 1`).Scan(&value))
			require.Equal(t, "sentinel", value)
		})
	}
}

// Both startup handles observe a pending migration before either applies it.
// Driving applyMigration directly makes that interleaving deterministic without
// timing sleeps or production hooks. A stale precheck must never replay ALTER.
func TestMigrationConcurrentStalePrecheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.db")
	a := openContentionDB(t, path, "WAL")
	b := openContentionDB(t, path, "WAL")
	_, err := a.ExecContext(t.Context(), `CREATE TABLE probe (value TEXT); INSERT INTO probe VALUES ('sentinel')`)
	require.NoError(t, err)
	calls := 0
	migration := Migration{
		ID: 1, Name: "001_probe", UpSQL: `ALTER TABLE probe ADD COLUMN added TEXT DEFAULT 'preserved'`,
		UpFunc: func(context.Context, *sql.DB) error { calls++; return nil },
	}
	ma := NewMigrationManagerWithMigrations(a, []Migration{migration})
	mb := NewMigrationManagerWithMigrations(b, []Migration{migration})
	require.NoError(t, ma.createMigrationsTable(t.Context()))
	for _, manager := range []*MigrationManager{ma, mb} {
		applied, err := manager.isMigrationApplied(t.Context(), migration.Name)
		require.NoError(t, err)
		require.False(t, applied)
	}
	require.NoError(t, ma.applyMigration(t.Context(), &migration))
	err = mb.applyMigration(t.Context(), &migration)
	t.Logf("second migration after stale precheck: %v", err)
	require.NoError(t, err)
	var value, added string
	require.NoError(t, a.QueryRowContext(t.Context(), `SELECT value, added FROM probe`).Scan(&value, &added))
	require.Equal(t, "sentinel", value)
	require.Equal(t, "preserved", added)
	require.Equal(t, 1, calls, "postcommit migration function must not replay")
}

func TestMigrationAcquiresWriterBeforeSchemaRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.db")
	a := openContentionDB(t, path, "WAL")
	b := openContentionDB(t, path, "WAL")
	_, err := b.ExecContext(t.Context(), `PRAGMA busy_timeout = 0`)
	require.NoError(t, err)
	manager := NewMigrationManagerWithMigrations(a, nil)
	require.NoError(t, manager.createMigrationsTable(t.Context()))
	migration := Migration{ID: 1, Name: "001_lock", UpTxFunc: func(ctx context.Context, tx *sql.Tx) error {
		// No migration SQL has run yet: only the manager's no-op acquired this
		// writer lock. Another handle cannot write over its schema snapshot.
		_, err := b.ExecContext(ctx, `INSERT INTO migrations VALUES (2, 'other', '', '')`)
		require.Error(t, err)
		var coded interface{ Code() int }
		require.ErrorAs(t, err, &coded)
		require.Equal(t, 5, coded.Code()&0xff)
		return nil
	}}
	require.NoError(t, manager.applyMigration(t.Context(), &migration))
}
