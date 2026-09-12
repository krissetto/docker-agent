package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingRecallStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "recall.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			sess := New(WithID("recall"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			for _, id := range []string{"first", "recalled", "last"} {
				msg := UserMessage(id)
				msg.Pending, msg.Accepted, msg.TurnID = true, true, id
				_, err := store.AddMessage(t.Context(), sess.ID, msg)
				require.NoError(t, err)
			}
			require.NoError(t, store.AddSummary(t.Context(), sess.ID, Item{Summary: "summary", FirstKeptEntry: 3}))
			// Promotion creates sparse SQLite positions and moves the logical boundary.
			require.NoError(t, store.PromotePendingUserMessage(t.Context(), sess.ID, "first"))
			deleter := store.(PendingMessageDeleter)
			require.ErrorIs(t, deleter.DeletePendingUserMessage(t.Context(), sess.ID, "first"), ErrNotFound)
			require.ErrorIs(t, deleter.DeletePendingUserMessage(t.Context(), "other", "recalled"), ErrNotFound)
			require.NoError(t, deleter.DeletePendingUserMessage(t.Context(), sess.ID, "recalled"))
			require.ErrorIs(t, deleter.DeletePendingUserMessage(t.Context(), sess.ID, "recalled"), ErrNotFound)
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			items := loaded.MessagesSnapshot()
			require.Len(t, items, 3)
			assert.Equal(t, "last", items[0].Message.TurnID)
			assert.Equal(t, 1, items[1].FirstKeptEntry)
			assert.Equal(t, "first", items[2].Message.TurnID)
			// Inserting after a sparse deletion must preserve transcript order.
			_, err = store.AddMessage(t.Context(), sess.ID, UserMessage("next"))
			require.NoError(t, err)
			loaded, err = store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, "next", loaded.MessagesSnapshot()[3].Message.Message.Content)
		})
	}
}

func TestPendingRecallSQLiteRollback(t *testing.T) {
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "recall.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := New(WithID("rollback"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	msg := UserMessage("keep")
	msg.Pending, msg.Accepted, msg.TurnID = true, true, "turn"
	_, err = store.AddMessage(t.Context(), sess.ID, msg)
	require.NoError(t, err)
	require.NoError(t, store.AddSummary(t.Context(), sess.ID, Item{Summary: "summary", FirstKeptEntry: 1}))
	sqlite := store.(*SQLiteSessionStore)
	_, err = sqlite.db.ExecContext(t.Context(), `CREATE TRIGGER fail_recall BEFORE UPDATE OF first_kept_entry ON session_items BEGIN SELECT RAISE(ABORT, 'recall failed'); END`)
	require.NoError(t, err)
	require.Error(t, sqlite.DeletePendingUserMessage(t.Context(), sess.ID, "turn"))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	items := loaded.MessagesSnapshot()
	require.Len(t, items, 2)
	assert.True(t, items[0].Message.Pending)
	assert.Equal(t, 1, items[1].FirstKeptEntry)
}
