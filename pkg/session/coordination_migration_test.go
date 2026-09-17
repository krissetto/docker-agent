package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestCoordinationSchemaUpgradesPreviouslyAppliedVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"baseline33", "early34", "inputmode34", "complete34", "complete35"} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgrade.db")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			db.SetMaxOpenConns(1)
			_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
			require.NoError(t, err)
			migrations := getAllMigrations()
			prior := append([]Migration(nil), migrations[:33]...)
			if version != "baseline33" {
				partial := migrations[33]
				if version != "complete34" && version != "complete35" {
					for _, statement := range []string{
						"ALTER TABLE sessions ADD COLUMN execution_settings TEXT NOT NULL DEFAULT '{}';",
						"ALTER TABLE session_items ADD COLUMN write_id TEXT NOT NULL DEFAULT '';",
						"ALTER TABLE session_items ADD COLUMN write_hash TEXT NOT NULL DEFAULT '';",
						"CREATE UNIQUE INDEX session_items_write_id ON session_items(session_id, write_id) WHERE write_id != '';",
					} {
						partial.UpSQL = strings.ReplaceAll(partial.UpSQL, statement, "")
					}
					if version == "early34" {
						partial.UpSQL = strings.ReplaceAll(partial.UpSQL, "ALTER TABLE session_items ADD COLUMN actor_input_mode TEXT NOT NULL DEFAULT '';", "")
					}
				}
				prior = append(prior, partial)
				if version == "complete35" {
					prior = append(prior, migrations[34])
				}
			}
			require.NoError(t, NewMigrationManagerWithMigrations(db, prior).InitializeMigrations(t.Context()))
			_, err = db.ExecContext(t.Context(), `INSERT INTO sessions(id, origin, created_at) VALUES ('preserved', 'run', '2026-01-01T00:00:00Z')`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO session_items(session_id,position,item_type,message_json) VALUES ('preserved', 0, 'message', '{"role":"user","content":"<system_info>original history</system_info>"}')`)
			require.NoError(t, err)
			if version == "early34" || version == "inputmode34" {
				var value string
				err := db.QueryRowContext(t.Context(), `SELECT write_hash FROM session_items LIMIT 1`).Scan(&value)
				require.ErrorContains(t, err, "no such column: write_hash")
			}
			if version != "baseline33" {
				_, err = db.ExecContext(t.Context(), `INSERT INTO sessions(id, origin, parent_id, created_at) VALUES ('child', 'run', 'preserved', '2026-01-01T00:00:00Z');
                  INSERT INTO child_records(session_id,root_session_id,parent_session_id,revision,record) VALUES ('child','preserved','preserved',1,'{}');
                  INSERT INTO child_reports(id,parent_session_id,child_session_id,turn_id,revision,content) VALUES ('report','preserved','child','turn',1,'preserved report')`)
				require.NoError(t, err)
			}
			if version == "inputmode34" || version == "complete34" || version == "complete35" {
				_, err = db.ExecContext(t.Context(), `UPDATE session_items SET actor_input_mode = 'steer' WHERE session_id = 'preserved'`)
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())
			store, err := newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			defer store.Close()
			loaded, err := store.GetSession(t.Context(), "preserved")
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 1)
			assert.Equal(t, "<system_info>original history</system_info>", loaded.Messages[0].Message.Message.Content)
			assert.Empty(t, loaded.Messages[0].Message.InputOrigin)
			assert.Empty(t, loaded.Messages[0].Message.SenderID)
			assert.Empty(t, loaded.Messages[0].Message.SenderName)
			assert.False(t, loaded.Messages[0].Message.Implicit)
			if version == "inputmode34" || version == "complete34" || version == "complete35" {
				assert.Equal(t, "steer", loaded.Messages[0].Message.InputMode)
			}
			if version != "baseline33" {
				var content string
				require.NoError(t, store.(*SQLiteSessionStore).db.QueryRowContext(t.Context(), `SELECT content FROM child_reports WHERE id = 'report'`).Scan(&content))
				assert.Equal(t, "preserved report", content)
				var count int
				require.NoError(t, store.(*SQLiteSessionStore).db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM child_records WHERE session_id = 'child'`).Scan(&count))
				assert.Equal(t, 1, count)
			}
			message := UserMessage("new input")
			message.InputMode = "submit"
			appender := store.(ItemAppender)
			id, err := appender.AppendItem(t.Context(), loaded.ID, "stable-write", Item{Message: message})
			require.NoError(t, err)
			retryID, err := appender.AppendItem(t.Context(), loaded.ID, "stable-write", Item{Message: message})
			require.NoError(t, err)
			assert.Equal(t, id, retryID)
			var maxVersion int
			require.NoError(t, store.(*SQLiteSessionStore).db.QueryRowContext(t.Context(), `SELECT MAX(id) FROM migrations`).Scan(&maxVersion))
			assert.Equal(t, 37, maxVersion)
			require.NoError(t, NewMigrationManager(store.(*SQLiteSessionStore).db).InitializeMigrations(t.Context()))
		})
	}
}

func TestTransactionalMigrationFailureDoesNotRecordVersion(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions(id TEXT PRIMARY KEY); CREATE TABLE session_items(id INTEGER PRIMARY KEY, session_id TEXT)`)
	require.NoError(t, err)
	manager := NewMigrationManagerWithMigrations(db, []Migration{{ID: 35, Name: "schema-repair", UpTxFunc: func(ctx context.Context, tx *sql.Tx) error {
		if err := completeChildCoordinationSchema(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected failure after transactional DDL")
	}}})
	require.Error(t, manager.InitializeMigrations(t.Context()))
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM migrations WHERE id = 35`).Scan(&count))
	assert.Zero(t, count)
	var hash string
	err = db.QueryRowContext(t.Context(), `SELECT write_hash FROM session_items`).Scan(&hash)
	require.ErrorContains(t, err, "no such column: write_hash")
	manager = NewMigrationManagerWithMigrations(db, []Migration{{ID: 35, Name: "schema-repair", UpTxFunc: completeChildCoordinationSchema}})
	require.NoError(t, manager.InitializeMigrations(t.Context()))
	require.NoError(t, manager.InitializeMigrations(t.Context()))
}
