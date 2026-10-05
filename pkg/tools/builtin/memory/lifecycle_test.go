package memory

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/memory/database"
)

type closingDB struct {
	MockDB

	closes   atomic.Int32
	closeErr error
}

func (db *closingDB) Close() error { db.closes.Add(1); return db.closeErr }

func TestBackendOwnership(t *testing.T) {
	t.Parallel()
	for _, owned := range []bool{false, true} {
		db := &closingDB{}
		var ts *ToolSet
		if owned {
			ts = NewOwnedWithPath(db, "db")
		} else {
			ts = NewWithPath(db, "db")
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() { require.NoError(t, ts.Close()) })
		}
		wg.Wait()
		expected := int32(0)
		if owned {
			expected = 1
		}
		require.Equal(t, expected, db.closes.Load())
	}
	db := &closingDB{}
	require.NoError(t, New(db).Close())
	require.Zero(t, db.closes.Load())
}

func TestFactoryBackendOwnershipAndFailureCleanup(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		db := &closingDB{}
		failure := errors.New("open failed")
		ts, err := CreateToolSetWithBackend(latest.Toolset{Path: filepath.Join(t.TempDir(), "memory.db")}, "", &config.RuntimeConfig{}, "test", func(string) (DB, error) {
			if fail {
				return db, failure
			}
			return db, nil
		})
		if fail {
			require.ErrorIs(t, err, failure)
			require.Nil(t, ts)
		} else {
			require.NoError(t, err)
			require.NoError(t, ts.(*ToolSet).Close())
			require.NoError(t, ts.(*ToolSet).Close())
		}
		require.EqualValues(t, 1, db.closes.Load())
	}
}

func TestDefaultBackendIsClosedByToolset(t *testing.T) {
	t.Parallel()
	ts, err := CreateToolSet(latest.Toolset{Path: filepath.Join(t.TempDir(), "memory.db")}, "", &config.RuntimeConfig{}, "test")
	require.NoError(t, err)
	toolset := ts.(*ToolSet)
	require.NotNil(t, toolset.owned)
	require.NoError(t, toolset.db.AddMemory(t.Context(), database.UserMemory{ID: "test", Memory: "value"}))
	require.NoError(t, toolset.Close())
	require.NoError(t, toolset.Close())
}
