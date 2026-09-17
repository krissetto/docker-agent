package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// newSQLiteStoreForTest opens a file-backed SQLite store the same way
// sqlitestore.New does on its happy path. In-package tests cannot import
// pkg/session/sqlitestore without an import cycle, and none of them exercise
// its backup/recovery logic (that is tested in sqlitestore itself).
func newSQLiteStoreForTest(t *testing.T, path string) (Store, error) {
	t.Helper()

	db, err := sqliteutil.OpenDBWithImmediateTransactions(t.Context(), path)
	if err != nil {
		return nil, err
	}

	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	if err != nil {
		require.NoError(t, db.Close())
		return nil, err
	}
	return store, nil
}
