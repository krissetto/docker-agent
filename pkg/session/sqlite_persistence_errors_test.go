package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

type sqliteResultCodeError int

func (e sqliteResultCodeError) Error() string { return fmt.Sprintf("synthetic SQLite code %d", e) }
func (e sqliteResultCodeError) Code() int     { return int(e) }

func TestSQLiteExtendedErrorClassification(t *testing.T) {
	for _, code := range []int{5, 6, 261, 517, 262} {
		err := fmt.Errorf("persistence: %w", sqliteResultCodeError(code))
		classified := classifySQLiteError(err)
		require.True(t, IsTemporary(classified), "code %d", code)
		require.ErrorIs(t, classified, err)
		require.Same(t, classified, classifySQLiteError(classified), "classification must be idempotent")
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, sqliteResultCodeError(19), errors.New("permanent")} {
		require.Equal(t, err, classifySQLiteError(err))
		require.False(t, IsTemporary(classifySQLiteError(err)))
	}
}

func TestSQLitePersistenceBoundariesClassifyBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.db")
	db := openContentionDB(t, path, "WAL")
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `PRAGMA busy_timeout = 0`)
	require.NoError(t, err)
	sess := New(WithID("sentinel"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	id, err := store.AddMessage(t.Context(), sess.ID, UserMessage("sentinel"))
	require.NoError(t, err)
	holder := openContentionDB(t, path, "WAL")
	tx, err := holder.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `UPDATE sessions SET title = title WHERE id = ?`, sess.ID)
	require.NoError(t, err)
	for name, write := range map[string]func() error{
		"add":            func() error { return store.AddSession(t.Context(), New(WithID("other"))) },
		"update":         func() error { return store.UpdateSession(t.Context(), sess) },
		"delete":         func() error { return store.DeleteSession(t.Context(), sess.ID) },
		"star":           func() error { return store.SetSessionStarred(t.Context(), sess.ID, true) },
		"title":          func() error { return store.UpdateSessionTitle(t.Context(), sess.ID, "new") },
		"tokens":         func() error { return store.UpdateSessionTokens(t.Context(), sess.ID, 1, 2, 3) },
		"message":        func() error { _, err := store.AddMessage(t.Context(), sess.ID, UserMessage("new")); return err },
		"update-message": func() error { return store.UpdateMessage(t.Context(), sess.ID, id, UserMessage("new")) },
		"summary":        func() error { return store.AddSummary(t.Context(), sess.ID, Item{Summary: "new"}) },
		"error":          func() error { return store.AddError(t.Context(), sess.ID, &Error{}) },
		"compaction":     func() error { return store.PersistCompaction(t.Context(), sess, 1, 2, Item{Summary: "new"}) },
		"subsession":     func() error { return store.AddSubSession(t.Context(), sess.ID, New(WithID("child"))) },
		"todos":          func() error { return store.SaveTodos(t.Context(), sess.ID, nil) },
		"tree":           func() error { return store.SaveTree(t.Context(), sess.ID, subagent.Snapshot{}) },
		"append": func() error {
			_, err := store.AppendItem(t.Context(), sess.ID, "write", Item{Message: UserMessage("new")})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := write()
			require.Error(t, err)
			require.True(t, IsTemporary(err), "%T: %v", err, err)
		})
	}
	require.NoError(t, tx.Rollback())
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	require.Equal(t, "sentinel", loaded.Messages[0].Message.Message.Content)
	require.NoError(t, store.UpdateSessionTitle(t.Context(), sess.ID, "reused"))
}

func TestIsTemporaryContextPolicy(t *testing.T) {
	for _, contextErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(contextErr.Error(), func(t *testing.T) {
			for _, err := range []error{contextErr, fmt.Errorf("caller: %w", contextErr)} {
				require.False(t, IsTemporary(err))
				require.ErrorIs(t, err, contextErr)
			}
			explicit := &TemporaryError{Err: contextErr}
			for _, err := range []error{explicit, fmt.Errorf("owned attempt: %w", explicit)} {
				require.True(t, IsTemporary(err))
				require.ErrorIs(t, err, contextErr)
			}
		})
	}
}
