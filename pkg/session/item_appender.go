package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var ErrWriteConflict = errors.New("session append write ID reused with different content")

// ItemAppender makes retries of one immutable append safe after an uncertain
// acknowledgment. Later message updates do not change the original fingerprint.
// Stores lacking this capability cannot promise idempotent append retries.
type ItemAppender interface {
	AppendItem(ctx context.Context, sessionID, writeID string, item Item) (int64, error)
}

type itemAppendReceipt struct {
	id   int64
	hash string
}

type itemAppendKey struct{ sessionID, writeID string }

func prepareAppend(item Item) (Item, string, error) {
	kinds := 0
	for _, present := range []bool{item.Message != nil, item.Summary != "", item.Error != nil, item.Termination != nil, item.SubSession != nil} {
		if present {
			kinds++
		}
	}
	if kinds != 1 || item.SubSession != nil {
		return Item{}, "", errors.New("append requires exactly one message, summary, error or termination")
	}
	// Round-trip only persisted data; excludes live attachment markers and IDs.
	data, err := json.Marshal(item)
	if err != nil {
		return Item{}, "", err
	}
	var frozen Item
	if err := json.Unmarshal(data, &frozen); err != nil {
		return Item{}, "", err
	}
	hash := sha256.Sum256(data)
	return frozen, hex.EncodeToString(hash[:]), nil
}

func (s *InMemorySessionStore) AppendItem(ctx context.Context, sessionID, writeID string, item Item) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if sessionID == "" || writeID == "" {
		return 0, ErrEmptyID
	}
	frozen, hash, err := prepareAppend(item)
	if err != nil {
		return 0, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	key := itemAppendKey{sessionID, writeID}
	if receipt, ok := s.appendReceipts[key]; ok {
		if receipt.hash != hash {
			return 0, ErrWriteConflict
		}
		return receipt.id, nil
	}
	stored, ok := s.sessions.Load(sessionID)
	if !ok {
		return 0, ErrNotFound
	}
	id := s.messageID.Add(1)
	if frozen.Message != nil {
		frozen.Message.ID = id
	}
	stored.mu.Lock()
	stored.Messages = append(stored.Messages, frozen)
	stored.mu.Unlock()
	if s.appendReceipts == nil {
		s.appendReceipts = make(map[itemAppendKey]itemAppendReceipt)
	}
	s.appendReceipts[key] = itemAppendReceipt{id: id, hash: hash}
	return id, nil
}

func (s *SQLiteSessionStore) AppendItem(ctx context.Context, sessionID, writeID string, item Item) (id int64, err error) {
	defer func() { err = classifySQLiteContextError(ctx, err) }()
	if sessionID == "" || writeID == "" {
		return 0, ErrEmptyID
	}
	frozen, hash, err := prepareAppend(item)
	if err != nil {
		return 0, err
	}
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var originalHash string
	err = tx.QueryRowContext(ctx, `SELECT id, write_hash FROM session_items WHERE session_id = ? AND write_id = ?`, sessionID, writeID).Scan(&id, &originalHash)
	if err == nil {
		if originalHash != hash {
			return 0, ErrWriteConflict
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?`, sessionID).Scan(&position); err != nil {
		return 0, err
	}
	if err := s.addItemTx(ctx, tx, sessionID, position, frozen); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_items SET write_id = ?, write_hash = ? WHERE id = ? AND session_id = ?`, writeID, hash, id, sessionID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

var (
	_ ItemAppender = (*InMemorySessionStore)(nil)
	_ ItemAppender = (*SQLiteSessionStore)(nil)
)
