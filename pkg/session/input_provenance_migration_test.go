package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestInputProvenanceMigrationRollsBack(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	catalog := getAllMigrations()
	require.NoError(t, NewMigrationManagerWithMigrations(db, catalog[:35]).InitializeMigrations(t.Context()))
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions(id, origin, created_at) VALUES ('parent', 'run', '2026-01-01T00:00:00Z');
		INSERT INTO session_items(session_id, position, item_type, message_json, actor_input_mode, write_id, write_hash)
		VALUES ('parent', 0, 'message', '{"role":"user","content":"<system_info>literal</system_info>"}', 'runtime_note', 'original-write', 'original-hash')`)
	require.NoError(t, err)
	migration := catalog[35]
	migration.UpTxFunc = func(context.Context, *sql.Tx) error { return errors.New("injected migration failure") }
	require.ErrorContains(t, NewMigrationManagerWithMigrations(db, []Migration{migration}).InitializeMigrations(t.Context()), "injected migration failure")
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 36`).Scan(&count))
	assert.Zero(t, count)
	for _, column := range []string{"input_origin", "sender_id", "sender_name"} {
		var value string
		err := db.QueryRowContext(t.Context(), "SELECT "+column+" FROM session_items").Scan(&value)
		require.ErrorContains(t, err, "no such column")
	}
	require.NoError(t, db.Close())
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	loaded, err := store.GetSession(t.Context(), "parent")
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	assert.Equal(t, "<system_info>literal</system_info>", loaded.Messages[0].Message.Message.Content)
	assert.Equal(t, "runtime_note", loaded.Messages[0].Message.InputMode)
	assert.Empty(t, loaded.Messages[0].Message.InputOrigin)
	assert.Empty(t, loaded.Messages[0].Message.SenderID)
	assert.Empty(t, loaded.Messages[0].Message.SenderName)
	var writeID, writeHash string
	require.NoError(t, store.(*SQLiteSessionStore).db.QueryRowContext(t.Context(), `SELECT write_id, write_hash FROM session_items WHERE session_id = 'parent'`).Scan(&writeID, &writeHash))
	assert.Equal(t, "original-write", writeID)
	assert.Equal(t, "original-hash", writeHash)
	require.NoError(t, NewMigrationManager(store.(*SQLiteSessionStore).db).InitializeMigrations(t.Context()))
}

func TestInputProvenanceMigrationPreservesReportAcknowledgements(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "acknowledgements.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	require.NoError(t, NewMigrationManagerWithMigrations(db, getAllMigrations()[:35]).InitializeMigrations(t.Context()))
	_, err = db.ExecContext(t.Context(), `INSERT INTO sessions(id, origin, created_at) VALUES ('parent', 'run', '2026-01-01T00:00:00Z'), ('child', 'run', '2026-01-01T00:00:00Z');
		INSERT INTO session_items(id, session_id, position, item_type, message_json, actor_input_mode, actor_turn_id, actor_accepted)
		VALUES (71, 'parent', 0, 'message', '{"role":"user","content":"<system_info>historic ambiguous report</system_info>"}', 'runtime_note', 'report:legacy-revision-7', 1);
		INSERT INTO child_records(session_id, root_session_id, parent_session_id, revision, record) VALUES ('child', 'parent', 'parent', 7, '{}');
		INSERT INTO child_reports(id, parent_session_id, child_session_id, turn_id, revision, content, message_id)
		VALUES ('legacy-revision-7', 'parent', 'child', 'turn', 7, '', 71), ('pending-report', 'parent', 'child', 'turn-next', 8, 'outbox payload', NULL)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cs := store.(CoordinationStore)
	probe, err := cs.AcceptReport(t.Context(), "parent", "legacy-revision-7", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(71), probe.MessageID)
	assert.False(t, probe.Created)
	require.NotNil(t, probe.Message)
	assert.False(t, probe.Message.Pending)
	assert.False(t, probe.Message.Implicit)
	assert.Empty(t, probe.Message.InputOrigin)
	assert.Equal(t, "<system_info>historic ambiguous report</system_info>", probe.Message.Message.Content)
	pending, err := cs.PendingReports(t.Context(), "parent")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "pending-report", pending[0].ID)
	assert.Equal(t, "outbox payload", pending[0].Content)
	var content string
	require.NoError(t, store.(*SQLiteSessionStore).db.QueryRowContext(t.Context(), `SELECT content FROM child_reports WHERE id = 'legacy-revision-7'`).Scan(&content))
	assert.Empty(t, content)
}
