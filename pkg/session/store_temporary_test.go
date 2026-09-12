package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestSQLiteBusyMapsToTemporaryContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	db1, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db1.Close()
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db1)
	require.NoError(t, err)
	defer store.Close()
	sess := New(WithID("busy"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	pending := UserMessage("A")
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "A"
	_, err = store.AddMessage(t.Context(), sess.ID, pending)
	require.NoError(t, err)

	db2, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db2.Close()
	conn, err := db2.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	err = store.PromotePendingUserMessage(t.Context(), sess.ID, "A")
	require.Error(t, err)
	assert.True(t, IsTemporary(err), "%T: %v", err, err)
	var temporary *TemporaryError
	require.ErrorAs(t, err, &temporary)
	require.Error(t, temporary.Unwrap())
	_, err = conn.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK")
	require.NoError(t, err)
}
