// Package sqlitestore provides the file-backed SQLite implementation of
// session.Store. It lives in its own package so that pkg/session — and
// through it pkg/runtime — does not link the modernc.org/sqlite driver into
// embedders that only use the in-memory store or bring their own database
// (see session.NewSQLiteSessionStoreFromDB).
package sqlitestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// New creates a new SQLite session store backed by a file at path. If
// opening a corrupt database fails, it is moved aside to <path>.bak and a
// fresh one is created. Migration failures never reset an existing database.
func New(ctx context.Context, path string) (session.Store, error) {
	store, err := open(ctx, path)
	if err != nil {
		// A migration failure can reflect valid history that needs attention,
		// not a corrupt file. Its transaction rolled back; preserve the source
		// for a later retry rather than silently resetting it. A version mismatch
		// likewise requires an upgrade, not a fresh database.
		if errors.Is(err, session.ErrNewerDatabase) || errors.Is(err, session.ErrMigrationFailed) {
			return nil, err
		}

		// Don't attempt recovery if we couldn't even open/create the database file
		// (e.g., permission denied, read-only filesystem, missing directory).
		// The backup+retry dance can't fix a filesystem-level problem, and would just
		// wrap the real error in a confusing "migration failed even after database reset"
		// message.
		if sqliteutil.IsCantOpenError(err) {
			return nil, err
		}

		// Don't attempt recovery for transient errors: a canceled context
		// (e.g. Ctrl-C during startup) or a BUSY/LOCKED database (e.g. a
		// second docker-agent instance holding a write lock). A fresh database
		// can't fix those, and the reset would silently discard a perfectly
		// healthy session history.
		if sqliteutil.IsTransientError(err) {
			return nil, err
		}

		// If migrations failed, try to recover by backing up the database and starting fresh
		slog.WarnContext(ctx, "Failed to open session store, attempting recovery", "error", err)

		backupErr := backupDatabase(path)
		if backupErr != nil {
			// Return the original error if backup failed
			slog.ErrorContext(ctx, "Failed to backup database for recovery", "error", backupErr)
			return nil, fmt.Errorf("migration failed: %w (backup also failed: %w)", err, backupErr)
		}

		// Try again with a fresh database
		store, err = open(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("migration failed even after database reset: %w", err)
		}

		slog.InfoContext(ctx, "Successfully recovered session store with fresh database")
	}

	return store, nil
}

// open opens the database and runs migrations
func open(ctx context.Context, path string) (*session.SQLiteSessionStore, error) {
	db, err := sqliteutil.OpenDBWithImmediateTransactions(ctx, path)
	if err != nil {
		return nil, err
	}

	store, err := session.NewSQLiteSessionStoreFromDB(ctx, db)
	if err != nil {
		db.Close()
		if sqliteutil.IsCantOpenError(err) {
			return nil, sqliteutil.DiagnoseDBOpenError(path, err)
		}
		return nil, err
	}

	return store, nil
}

// backupDatabase moves the database file (and related WAL files) to a backup
func backupDatabase(path string) error {
	backupPath := path + ".bak"
	if _, err := os.Stat(backupPath); err == nil {
		// A backup from an earlier recovery already exists; renaming over it
		// would destroy the last known-good copy, so keep both.
		backupPath = fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
	}

	slog.Info("Backing up database", "from", path, "to", backupPath)

	// Move the main database file
	if err := os.Rename(path, backupPath); err != nil {
		if os.IsNotExist(err) {
			// No database file to backup, that's fine
			return nil
		}
		return fmt.Errorf("failed to move database file: %w", err)
	}

	// Also move WAL and SHM files if they exist (SQLite WAL mode artifacts)
	walPath := path + "-wal"
	if _, err := os.Stat(walPath); err == nil {
		if err := os.Rename(walPath, backupPath+"-wal"); err != nil {
			slog.Warn("Failed to move WAL file", "error", err)
		}
	}

	shmPath := path + "-shm"
	if _, err := os.Stat(shmPath); err == nil {
		if err := os.Rename(shmPath, backupPath+"-shm"); err != nil {
			slog.Warn("Failed to move SHM file", "error", err)
		}
	}

	return nil
}
