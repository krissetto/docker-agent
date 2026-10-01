package sqliteutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenDB_UsesWALWithFullSynchronous(t *testing.T) {
	for _, opener := range []struct {
		name string
		open func(context.Context, string) (*sql.DB, error)
	}{
		{name: "OpenDB", open: OpenDB},
		{name: "OpenDBWithImmediateTransactions", open: OpenDBWithImmediateTransactions},
	} {
		t.Run(opener.name, func(t *testing.T) {
			for _, sandbox := range []string{"", "synthetic-vm"} {
				t.Run("sandbox="+sandbox, func(t *testing.T) {
					t.Setenv("SANDBOX_VM_ID", sandbox)
					for _, existingMode := range []string{"new", "delete", "wal"} {
						t.Run(existingMode, func(t *testing.T) {
							path := filepath.Join(t.TempDir(), "session.db")
							if existingMode != "new" {
								seed, err := sql.Open("sqlite", path+"?_pragma=journal_mode("+existingMode+")")
								require.NoError(t, err)
								t.Cleanup(func() { require.NoError(t, seed.Close()) })
								var mode string
								require.NoError(t, seed.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
								require.Equal(t, existingMode, mode)
								_, err = seed.ExecContext(t.Context(), "CREATE TABLE t (id INTEGER); INSERT INTO t VALUES (42)")
								require.NoError(t, err)
								require.NoError(t, seed.Close())
							}

							db, err := opener.open(t.Context(), path)
							require.NoError(t, err)
							t.Cleanup(func() { require.NoError(t, db.Close()) })

							var mode string
							require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
							assert.Equal(t, "wal", mode)
							var synchronous int
							require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous))
							assert.Equal(t, 2, synchronous, "FULL durability must be preserved")

							if existingMode != "new" {
								var id int
								require.NoError(t, db.QueryRowContext(t.Context(), "SELECT id FROM t").Scan(&id))
								assert.Equal(t, 42, id)
							}
						})
					}
				})
			}
		})
	}
}

func TestIsTransientError(t *testing.T) {
	t.Parallel()

	assert.False(t, IsTransientError(nil))
	assert.False(t, IsTransientError(errors.New("boom")))
	assert.True(t, IsTransientError(context.Canceled))
	assert.True(t, IsTransientError(context.DeadlineExceeded))
	assert.True(t, IsTransientError(fmt.Errorf("wrapped: %w", context.Canceled)))
}

func TestIsTransientError_SQLiteBusy(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := path + "?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)"

	writer, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer writer.Close()

	tx, err := writer.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // test cleanup, error irrelevant
	_, err = tx.ExecContext(t.Context(), "CREATE TABLE t (id INTEGER)")
	require.NoError(t, err)

	// A second connection hits the write lock and gets SQLITE_BUSY.
	blocked, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer blocked.Close()

	_, err = blocked.ExecContext(t.Context(), "CREATE TABLE u (id INTEGER)")
	require.Error(t, err)
	assert.True(t, IsTransientError(err), "SQLITE_BUSY should be transient: %v", err)
	assert.False(t, IsCantOpenError(err))
}

func TestCloseDB_WaitsForInFlightConnection(t *testing.T) {
	t.Parallel()

	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "close.db"))
	require.NoError(t, err)

	// Check out the single pooled connection and keep it busy until released.
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	released := make(chan struct{})
	go func() {
		<-released
		_ = conn.Close()
	}()

	closed := make(chan error, 1)
	go func() { closed <- CloseDB(db) }()

	select {
	case <-closed:
		t.Fatal("CloseDB returned while a connection was still checked out")
	case <-time.After(100 * time.Millisecond):
	}

	close(released)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(closeDrainTimeout):
		t.Fatal("CloseDB did not return after the connection was released")
	}
	assert.Zero(t, db.Stats().OpenConnections)
}

func TestCloseDB_IdleReturnsImmediately(t *testing.T) {
	t.Parallel()

	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "idle.db"))
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, CloseDB(db))
	assert.Less(t, time.Since(start), time.Second)
	assert.Zero(t, db.Stats().OpenConnections)
}
