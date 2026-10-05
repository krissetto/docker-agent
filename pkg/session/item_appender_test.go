package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestItemAppenderRetriesOriginalPayloadAfterUpdate(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			sess := New()
			require.NoError(t, store.AddSession(t.Context(), sess))
			appender := store.(ItemAppender)
			message := UserMessage("original")
			message.InputMode, message.TurnID = "submit", "turn"
			id, err := appender.AppendItem(t.Context(), sess.ID, "message-write", Item{Message: message})
			require.NoError(t, err)
			updated := cloneMessage(message)
			updated.Message.Content = "updated"
			require.NoError(t, store.UpdateMessage(t.Context(), sess.ID, id, updated))
			retryID, err := appender.AppendItem(t.Context(), sess.ID, "message-write", Item{Message: message})
			require.NoError(t, err)
			assert.Equal(t, id, retryID)
			_, err = appender.AppendItem(t.Context(), sess.ID, "message-write", Item{Message: updated})
			require.ErrorIs(t, err, ErrWriteConflict)
			summary := Item{Summary: "summary", FirstKeptEntry: 1, Cost: 0.2, Model: "model"}
			summaryID, err := appender.AppendItem(t.Context(), sess.ID, "summary-write", summary)
			require.NoError(t, err)
			retryID, err = appender.AppendItem(t.Context(), sess.ID, "summary-write", summary)
			require.NoError(t, err)
			assert.Equal(t, summaryID, retryID)
			_, err = appender.AppendItem(t.Context(), sess.ID, "empty", Item{})
			require.Error(t, err)
			_, err = appender.AppendItem(t.Context(), sess.ID, "ambiguous", Item{Message: message, Summary: "summary"})
			require.Error(t, err)
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 2)
			assert.Equal(t, "updated", loaded.Messages[0].Message.Message.Content)
			assert.Equal(t, "submit", loaded.Messages[0].Message.InputMode)
			assert.Equal(t, summary.Summary, loaded.Messages[1].Summary)
			assert.InDelta(t, summary.Cost, loaded.Messages[1].Cost, 1e-9)
		})
	}
}

func TestSQLiteItemAppenderRetryAfterRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "append.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	sess := New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	item := Item{Message: UserMessage("immutable input")}
	id, err := store.(ItemAppender).AppendItem(t.Context(), sess.ID, "write", item)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	retryID, err := store.(ItemAppender).AppendItem(t.Context(), sess.ID, "write", item)
	require.NoError(t, err)
	assert.Equal(t, id, retryID)
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Len(t, loaded.Messages, 1)
}

func TestWithdrawnAppendReceiptSurvivesPayloadRemoval(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := NewInMemorySessionStore()
			path := filepath.Join(t.TempDir(), "withdrawn.db")
			if kind == "sqlite" {
				var err error
				store, err = newSQLiteStoreForTest(t, path)
				require.NoError(t, err)
			}
			sess := New(WithID("withdrawn"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			message := UserMessage("original")
			message.Pending, message.Accepted, message.TurnID = true, true, "turn"
			item := NewMessageItem(message)
			_, err := store.(ItemAppender).AppendItem(t.Context(), sess.ID, "input:turn", item)
			require.NoError(t, err)
			require.NoError(t, store.(PendingInputWithdrawer).WithdrawPendingUserMessage(t.Context(), sess.ID, "turn"))
			require.NoError(t, store.(PendingInputWithdrawer).WithdrawPendingUserMessage(t.Context(), sess.ID, "turn"), "lost acknowledgment retry")
			if kind == "sqlite" {
				require.NoError(t, store.Close())
				store, err = newSQLiteStoreForTest(t, path)
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			_, err = store.(ItemAppender).AppendItem(t.Context(), sess.ID, "input:turn", item)
			require.ErrorIs(t, err, ErrInputWithdrawn)
			different := cloneMessage(message)
			different.Message.Content = "different"
			_, err = store.(ItemAppender).AppendItem(t.Context(), sess.ID, "input:turn", NewMessageItem(different))
			require.ErrorIs(t, err, ErrWriteConflict)
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Empty(t, loaded.MessagesSnapshot())
		})
	}
}
