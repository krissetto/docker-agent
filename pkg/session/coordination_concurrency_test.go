package session

import (
	"database/sql"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestInMemoryMetadataUpdatesDoNotLoseConcurrentMessages(t *testing.T) {
	t.Parallel()
	store := NewInMemorySessionStore()
	sess := New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	const count = 100
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			_, err := store.AddMessage(t.Context(), sess.ID, UserMessage("input"))
			assert.NoError(t, err)
		})
		wg.Go(func() { assert.NoError(t, store.UpdateSession(t.Context(), sess)) })
	}
	wg.Wait()
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, count)
	ids := make(map[int64]bool)
	for _, item := range loaded.Messages {
		require.NotZero(t, item.Message.ID)
		require.False(t, ids[item.Message.ID])
		ids[item.Message.ID] = true
	}
	assert.Empty(t, sess.Messages)
}

func TestSQLiteCoordinationDeletionWithoutForeignKeys(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/coordination.db")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root := New()
	require.NoError(t, store.AddSession(t.Context(), root))
	admission := coordinationAdmission(root.ID)
	require.NoError(t, store.AdmitChild(t.Context(), admission))
	require.NoError(t, store.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: admission.Record, Reports: []ChildReport{{ID: "report", ParentSessionID: root.ID, ChildSessionID: admission.Child.ID, TurnID: "turn", Content: "result"}}}))
	_, err = db.ExecContext(t.Context(), "PRAGMA foreign_keys = OFF")
	require.NoError(t, err)
	require.NoError(t, store.DeleteSession(t.Context(), root.ID))
	var records, reports int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM child_records").Scan(&records))
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM child_reports").Scan(&reports))
	assert.Zero(t, records)
	assert.Zero(t, reports)
}
