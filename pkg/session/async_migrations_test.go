package session

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// The upstream catalog is already deployed through generated-media migration
// 30. Async migrations must extend it without reusing its primary keys.
func TestAsyncMigrationsUpgradeUpstreamCatalog(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/upstream.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := t.Context()
	_, err = db.ExecContext(ctx, `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	catalog := getAllMigrations()
	require.NoError(t, NewMigrationManagerWithMigrations(db, catalog[:30]).InitializeMigrations(ctx))
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, title) VALUES ('upstream-session', 'preserved')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO generated_media_manifest (session_id, rel_path, mime_type, created_at) VALUES ('upstream-session', 'image.png', 'image/png', '2026-01-01')`)
	require.NoError(t, err)
	require.NoError(t, NewMigrationManager(db).InitializeMigrations(ctx))
	// Reopening an upgraded database must not repeat any migration.
	require.NoError(t, NewMigrationManager(db).InitializeMigrations(ctx))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migrations`).Scan(&count))
	require.Equal(t, len(catalog), count)
	var title, rootKind string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT title FROM sessions WHERE id = 'upstream-session'`).Scan(&title))
	require.Equal(t, "preserved", title)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT root_kind FROM generated_media_manifest WHERE session_id = 'upstream-session'`).Scan(&rootKind))
	require.Equal(t, "workspace", rootKind)
	_, err = db.ExecContext(ctx, `INSERT INTO session_items (session_id, position, item_type, actor_pending, actor_accepted, actor_turn_id) VALUES ('upstream-session', 0, 'message', 1, 1, 'pending-turn')`)
	require.NoError(t, err)
}
