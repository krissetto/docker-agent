package sqliteutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// OpenDB opens a SQLite database with recommended pragmas for concurrency and foreign key support.
// It configures the connection pool for serialized writes (MaxOpenConns=1).
func OpenDB(ctx context.Context, path string) (*sql.DB, error) {
	return openDB(ctx, path, false)
}

// OpenDBWithImmediateTransactions opens a database whose writable transactions
// acquire SQLite's writer lock before reading. Use it for read-then-write stores:
// a deferred snapshot cannot be upgraded after another handle commits, even with
// a busy timeout. Read-only transactions and other OpenDB clients are unchanged.
func OpenDBWithImmediateTransactions(ctx context.Context, path string) (*sql.DB, error) {
	return openDB(ctx, path, true)
}

func openDB(ctx context.Context, path string, immediate bool) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create database directory %q: %w", dir, err)
	}

	// Add query parameters for better concurrency handling and data integrity
	// _pragma=busy_timeout(5000): Wait up to 5 seconds if database is locked
	// _pragma=journal_mode(...): WAL outside sandboxes, DELETE inside (see journalMode)
	// _pragma=foreign_keys(1): Enable foreign key constraints (critical for ON DELETE CASCADE)
	busyTimeout := "5000"
	if immediate {
		// SQLite does not promptly interrupt its native busy handler. Session
		// writes retry only BEGIN acquisition in Go, with a context-aware bound.
		busyTimeout = "50"
	}
	dsn := path + "?_pragma=busy_timeout(" + busyTimeout + ")&_pragma=journal_mode(" + journalMode() + ")&_pragma=foreign_keys(1)"
	if immediate {
		dsn += "&_txlock=immediate"
	}

	// Only connection configuration is retried, before any schema or user
	// migration runs. Each failed handle is closed; Ping applies only the
	// idempotent DSN pragmas above. Other OpenDB clients retain their policy.
	started := time.Now()
	for {
		db, err := openConfiguredDB(ctx, dsn)
		if err == nil {
			return db, nil
		}
		if immediate && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if IsCantOpenError(err) {
			return nil, DiagnoseDBOpenError(path, err)
		}
		remaining := 5*time.Second - time.Since(started)
		if !immediate || !IsTransientError(err) || remaining <= 0 {
			return nil, err
		}
		timer := time.NewTimer(min(25*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func openConfiguredDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Serialize writes within this pool; independent handles still contend for
	// SQLite's writer lock and use the busy timeout.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Verify connection works (this will trigger file creation/open)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// journalMode picks the SQLite journal mode. WAL is preferred, but it relies
// on a shared-memory index (the -shm file) mmap'd by every connection. Inside
// a Docker sandbox the data dir is bind-mounted from the host (virtiofs),
// where that mapping is not coherent across the VM boundary and WAL corrupts
// the database. DELETE mode uses only ordinary file locks, which the mount
// forwards correctly; it also folds any leftover -wal file back into the main
// database on open.
func journalMode() string {
	// Mirrors environment.InSandbox; checked directly to keep sqliteutil
	// dependency-free.
	if os.Getenv("SANDBOX_VM_ID") != "" {
		return "DELETE"
	}
	return "WAL"
}

// closeDrainTimeout bounds how long CloseDB waits for in-use connections.
const closeDrainTimeout = 5 * time.Second

// CloseDB closes db and waits until every connection has actually been
// released. sql.DB.Close only closes idle connections: one checked out by an
// in-flight statement is closed when that statement returns, after Close has
// already come back. On Windows the database file cannot be deleted while
// such a connection still holds it, so callers that remove the directory
// right after closing (tests with t.TempDir, recovery paths) need the handle
// gone before Close returns. Waiting is bounded so a wedged statement cannot
// hang shutdown.
func CloseDB(db *sql.DB) error {
	err := db.Close()
	deadline := time.Now().Add(closeDrainTimeout)
	for db.Stats().OpenConnections > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return err
}

// IsCantOpenError checks if the error is a SQLite CANTOPEN error (code 14).
func IsCantOpenError(err error) bool {
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		return sqliteErr.Code() == sqlite3.SQLITE_CANTOPEN
	}
	return false
}

// IsTransientError reports whether err is a temporary condition that a retry
// (not a schema fix) would resolve: a canceled/expired context, or a SQLite
// BUSY/LOCKED error from a concurrent writer. The primary result code is
// compared after masking off extended-code bits (e.g. SQLITE_BUSY_SNAPSHOT).
func IsTransientError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	return false
}

// DiagnoseDBOpenError provides a more helpful error message when SQLite
// fails to open/create a database file.
func DiagnoseDBOpenError(path string, originalErr error) error {
	dir := filepath.Dir(path)

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cannot create database at %q: directory %q does not exist", path, dir)
		}
		return fmt.Errorf("cannot create database at %q: %w", path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("cannot create database at %q: %q is not a directory", path, dir)
	}

	return fmt.Errorf("cannot create database at %q: permission denied or file cannot be created in %q (original error: %w)", path, dir, originalErr)
}

// CheckpointAndClose runs a final WAL checkpoint and closes the database.
// The TRUNCATE checkpoint folds the -wal file back into the main database
// so it isn't left behind on disk after shutdown. A checkpoint failure is
// logged but does not prevent the close.
func CheckpointAndClose(ctx context.Context, db *sql.DB) error {
	ctx = context.WithoutCancel(ctx)
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.WarnContext(ctx, "Failed to checkpoint WAL before close", "error", err)
	}
	return db.Close()
}
