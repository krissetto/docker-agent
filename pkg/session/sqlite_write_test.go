package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSQLiteWriteAdmissionCancellationAndReuse(t *testing.T) {
	for _, sandbox := range []string{"", "synthetic-vm"} {
		t.Run("sandbox="+sandbox, func(t *testing.T) {
			t.Setenv("SANDBOX_VM_ID", sandbox)
			path := filepath.Join(t.TempDir(), "synthetic.db")
			opened, err := newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			store := opened.(*SQLiteSessionStore)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			other, err := newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			holder := other.(*SQLiteSessionStore)
			t.Cleanup(func() { require.NoError(t, holder.Close()) })
			sess := New(WithID("sentinel"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			for _, operation := range []string{"append", "title"} {
				t.Run(operation, func(t *testing.T) {
					tx, err := holder.db.BeginTx(t.Context(), nil)
					require.NoError(t, err)
					defer func() { _ = tx.Rollback() }()
					ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
					defer cancel()
					started := time.Now()
					if operation == "append" {
						_, err = store.AppendItem(ctx, sess.ID, "canceled", Item{Message: UserMessage("must not persist")})
					} else {
						err = store.UpdateSessionTitle(ctx, sess.ID, "must not persist")
					}
					elapsed := time.Since(started)
					t.Logf("operation=%s deadline=50ms elapsed=%s err=%v", operation, elapsed, err)
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.False(t, IsTemporary(err))
					require.Less(t, elapsed, 500*time.Millisecond, "deadline plus one native slice and scheduler allowance")
					require.NoError(t, tx.Rollback())
					require.NoError(t, store.UpdateSessionTitle(t.Context(), sess.ID, "reused"))
					require.Zero(t, store.db.Stats().InUse)
				})
			}
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Empty(t, loaded.Messages)
			require.Equal(t, "reused", loaded.Title)
			// The connection also remains reusable after SQLite has returned busy
			// several times, but the external writer releases before the budget.
			tx, err := holder.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			done := make(chan error, 1)
			go func() { done <- store.UpdateSessionTitle(t.Context(), sess.ID, "after-wait") }()
			select {
			case err := <-done:
				t.Fatalf("write returned before lock release: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
			require.NoError(t, tx.Rollback())
			require.NoError(t, <-done)
		})
	}
}

func TestSQLiteWriteAdmissionSustainedBusy(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "")
	path := filepath.Join(t.TempDir(), "synthetic.db")
	opened, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	store := opened.(*SQLiteSessionStore)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	other, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	holder := other.(*SQLiteSessionStore)
	t.Cleanup(func() { require.NoError(t, holder.Close()) })
	require.NoError(t, store.AddSession(t.Context(), New(WithID("sentinel"))))
	tx, err := holder.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	started := time.Now()
	err = store.UpdateSessionTitle(t.Context(), "sentinel", "must not persist")
	elapsed := time.Since(started)
	t.Logf("sustained busy elapsed=%s error=%v", elapsed, err)
	require.True(t, IsTemporary(err))
	require.GreaterOrEqual(t, elapsed, sqliteWriteWait)
	require.Less(t, elapsed, sqliteWriteWait+time.Second)
	require.NoError(t, tx.Rollback())
	require.NoError(t, store.UpdateSessionTitle(t.Context(), "sentinel", "reused"))
}

func TestSQLiteContextErrorClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, code := range []int{5, 6, 9, 517, 262} {
		err := classifySQLiteContextError(ctx, sqliteResultCodeError(code))
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, IsTemporary(err))
	}
	require.NoError(t, classifySQLiteContextError(ctx, nil), "successful commit stays successful")
	require.Equal(t, sqliteResultCodeError(19), classifySQLiteContextError(ctx, sqliteResultCodeError(19)))
}
