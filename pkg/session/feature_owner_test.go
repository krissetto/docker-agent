package session

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSQLiteFeatureStoresRejectMissingOwnerWithPooledConnections(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/pooled.db")
	require.NoError(t, err)
	db.SetMaxOpenConns(2)
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.ErrorIs(t, store.SaveTodos(t.Context(), "missing", []Todo{{ID: "todo_1"}}), ErrNotFound)
	require.ErrorIs(t, store.SaveTree(t.Context(), "missing", subagent.Snapshot{Version: 1}), ErrNotFound)
	var todos, trees int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_todos`).Scan(&todos))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM subagent_trees`).Scan(&trees))
	require.Zero(t, todos)
	require.Zero(t, trees)
}
