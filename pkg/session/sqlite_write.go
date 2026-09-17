package session

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	sqliteWriteWait       = 5 * time.Second
	sqliteWriteRetryDelay = 25 * time.Millisecond
)

// beginSQLiteWrite retries only lock acquisition, never a transaction body or
// commit. Production pools use immediate transactions and a 50ms SQLite busy
// timeout: short native waits keep cancellation responsive, while this bounded
// wait preserves the former five-second tolerance for another writer. SQLite's
// lock, not a process-local mutex, serializes independent handles.
func beginSQLiteWrite(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	started := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			if err := ctx.Err(); err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			return tx, nil
		}
		err = classifySQLiteContextError(ctx, err)
		var coded interface{ Code() int }
		if !errors.As(err, &coded) || (coded.Code()&0xff != 5 && coded.Code()&0xff != 6) {
			return nil, err
		}
		remaining := sqliteWriteWait - time.Since(started)
		if remaining <= 0 {
			return nil, err
		}
		timer := time.NewTimer(min(sqliteWriteRetryDelay, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// execSQLiteWrite gives single-statement mutators the same cancellable writer
// admission as multi-statement transactions. Results are exposed only after a
// successful commit; neither failed statements nor uncertain commits replay.
func execSQLiteWrite(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	tx, err := beginSQLiteWrite(ctx, db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	return result, nil
}
