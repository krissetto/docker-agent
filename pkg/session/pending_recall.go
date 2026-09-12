package session

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// PendingMessageDeleter is the optional durable pending-input recall capability.
// A missing or already promoted turn returns ErrNotFound without changing state.
type PendingMessageDeleter interface {
	DeletePendingUserMessage(ctx context.Context, sessionID, turnID string) error
}

// RemovePendingUserMessageByTurnID removes only unconsumed user input. Stable
// turn identity, rather than a remembered position, survives FIFO promotion.
func (s *Session) RemovePendingUserMessageByTurnID(turnID string) bool {
	if turnID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for position, item := range s.Messages {
		if item.Message == nil || !item.Message.Pending || item.Message.TurnID != turnID {
			continue
		}
		for i := range s.Messages {
			if s.Messages[i].FirstKeptEntry > position {
				s.Messages[i].FirstKeptEntry--
			}
		}
		s.Messages = slices.Delete(s.Messages, position, position+1)
		return true
	}
	return false
}

func (s *InMemorySessionStore) DeletePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" {
		return ErrEmptyID
	}
	sess, ok := s.sessions.Load(sessionID)
	if !ok || !sess.RemovePendingUserMessageByTurnID(turnID) {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteSessionStore) DeletePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	if turnID == "" {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT target.id,
		(SELECT COUNT(*) FROM session_items earlier WHERE earlier.session_id = target.session_id AND earlier.position < target.position)
		FROM session_items target WHERE target.session_id = ? AND target.actor_pending = 1 AND target.actor_turn_id = ?`, sessionID, turnID).Scan(&id, &position); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return classifySQLiteError(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_items WHERE id = ? AND session_id = ? AND actor_pending = 1`, id, sessionID); err != nil {
		return classifySQLiteError(err)
	}
	// Positions may already be sparse after promotion. Loaders order by these
	// keys; only logical summary boundaries need shifting after deletion.
	if _, err := tx.ExecContext(ctx, `UPDATE session_items SET first_kept_entry = first_kept_entry - 1
		WHERE session_id = ? AND summary_text IS NOT NULL AND first_kept_entry > ?`, sessionID, position); err != nil {
		return classifySQLiteError(err)
	}
	return classifySQLiteError(tx.Commit())
}
