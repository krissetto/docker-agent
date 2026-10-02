package sqlitestore

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestListSessionIDs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := New(t.Context(), path)
	require.NoError(t, err)
	now := time.Now().Truncate(time.Second)
	for _, s := range []*session.Session{
		{ID: "old-root", CreatedAt: now.Add(-time.Hour)},
		{ID: "new-root-b", CreatedAt: now},
		{ID: "new-root-a", CreatedAt: now},
		{ID: "child", ParentID: "old-root", CreatedAt: now.Add(time.Hour)},
	} {
		require.NoError(t, store.AddSession(t.Context(), s))
	}
	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	var want []string
	for _, summary := range summaries {
		want = append(want, summary.ID)
	}
	require.NoError(t, store.Close())
	specialPath := filepath.Join(t.TempDir(), "sessions ?#.db")
	require.NoError(t, os.Rename(path, specialPath))
	before, err := os.ReadFile(specialPath)
	require.NoError(t, err)
	ids, err := ListSessionIDs(t.Context(), specialPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"new-root-a", "new-root-b", "old-root"}, ids)
	assert.Equal(t, want, ids)
	after, err := os.ReadFile(specialPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "listing must not change the database")
}

func TestListSessionIDsEmptyAndMissing(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing", "zero-byte", "empty-store"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "nested", "session.db")
			switch kind {
			case "zero-byte":
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			case "empty-store":
				store, err := New(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, store.Close())
			}
			ids, err := ListSessionIDs(t.Context(), path)
			require.NoError(t, err)
			assert.Empty(t, ids)
			if kind == "missing" {
				assert.NoDirExists(t, filepath.Dir(path))
			}
		})
	}
}

func TestListSessionIDsLegacy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT);
		INSERT INTO sessions VALUES ('legacy', '[]', '2025-01-01T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	ids, err := ListSessionIDs(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, []string{"legacy"}, ids)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "listing must not migrate legacy databases")
}

func TestListSessionIDsCorruptPreserved(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "corrupt.db")
	contents := []byte("not a database")
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	ids, err := ListSessionIDs(t.Context(), path)
	require.Error(t, err)
	assert.Empty(t, ids)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, after)
	assert.NoFileExists(t, path+".bak")
}

func TestListSessionIDsReadsActiveWAL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := New(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	older := session.New()
	older.CreatedAt = time.Now().Add(-time.Hour)
	require.NoError(t, store.AddSession(t.Context(), older))
	newer := session.New()
	require.NoError(t, store.AddSession(t.Context(), newer))
	wal, err := os.Stat(path + "-wal")
	require.NoError(t, err)
	require.Positive(t, wal.Size())
	ids, err := ListSessionIDs(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, []string{newer.ID, older.ID}, ids)
}
