package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

var (
	ErrPendingMessageStale  = errors.New("pending message no longer editable")
	ErrPendingMessageLayout = errors.New("unsupported pending message text layout")
	ErrPendingMessageEmpty  = errors.New("content or multi_content is required")
)

// PendingMessageEditor updates an accepted admission without changing its identity.
type PendingMessageEditor interface {
	EditPendingUserMessage(ctx context.Context, sessionID, turnID, content string) error
}

// PendingUserMessageReplacement prepares a detached payload, preserving the
// leading text's attachment suffix and every non-text part produced by the App.
func PendingUserMessageReplacement(message *Message, content string) (*Message, error) {
	if message == nil || !message.Pending || !message.Accepted || message.InputOrigin != InputOriginUser || message.Message.Role != chat.MessageRoleUser {
		return nil, ErrPendingMessageStale
	}
	parts := message.Message.MultiContent
	if strings.TrimSpace(content) == "" && len(parts) == 0 {
		return nil, ErrPendingMessageEmpty
	}
	if len(parts) != 0 {
		first := parts[0]
		if first.Type != chat.MessagePartTypeText || first.Document != nil || first.ImageURL != nil || first.File != nil || !strings.HasPrefix(first.Text, message.Message.Content) {
			return nil, ErrPendingMessageLayout
		}
		suffix := strings.TrimPrefix(first.Text, message.Message.Content)
		if suffix != "" && !strings.HasPrefix(suffix, "\n") {
			return nil, ErrPendingMessageLayout
		}
		for _, part := range parts[1:] {
			if part.Type == chat.MessagePartTypeText || part.Text != "" {
				return nil, ErrPendingMessageLayout
			}
		}
	}
	next := cloneMessage(message)
	if len(parts) != 0 {
		next.Message.MultiContent[0].Text = content + strings.TrimPrefix(parts[0].Text, message.Message.Content)
	}
	next.Message.Content = content
	return next, nil
}

// ReplacePendingUserMessagePayload replaces an existing slot only. It is also
// safe when the in-memory store has already updated the shared session pointer.
func (s *Session) ReplacePendingUserMessagePayload(turnID, content string, parts []chat.MessagePart) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, item := range s.Messages {
		if item.Message == nil || item.Message.TurnID != turnID {
			continue
		}
		if index >= 0 || !item.Message.Pending || !item.Message.Accepted || item.Message.InputOrigin != InputOriginUser || item.Message.Message.Role != chat.MessageRoleUser {
			return false
		}
		index = i
	}
	if index < 0 || turnID == "" {
		return false
	}
	next := cloneMessage(s.Messages[index].Message)
	next.Message.Content = content
	next.Message.MultiContent = cloneChatMessage(chat.Message{MultiContent: parts}).MultiContent
	s.Messages[index].Message = next
	return true
}

func (s *InMemorySessionStore) EditPendingUserMessage(ctx context.Context, sessionID, turnID, content string) error {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" {
		return ErrEmptyID
	}
	sess, ok := s.sessions.Load(sessionID)
	if !ok || turnID == "" {
		return ErrPendingMessageStale
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	index := -1
	for i, item := range sess.Messages {
		if item.Message != nil && item.Message.TurnID == turnID {
			if index >= 0 {
				return ErrPendingMessageStale
			}
			index = i
		}
	}
	if index < 0 {
		return ErrPendingMessageStale
	}
	next, err := PendingUserMessageReplacement(sess.Messages[index].Message, content)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sess.Messages[index].Message = next
	return nil
}

func (s *SQLiteSessionStore) EditPendingUserMessage(ctx context.Context, sessionID, turnID, content string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	if turnID == "" {
		return ErrPendingMessageStale
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	var payload string
	if err := tx.QueryRowContext(ctx, `SELECT id, message_json FROM session_items
		WHERE session_id = ? AND actor_turn_id = ? AND actor_pending = 1 AND actor_accepted = 1 AND input_origin = ? AND item_type = 'message'
		AND (SELECT COUNT(*) FROM session_items WHERE session_id = ? AND actor_turn_id = ?) = 1`, sessionID, turnID, InputOriginUser, sessionID, turnID).Scan(&id, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPendingMessageStale
		}
		return classifySQLiteError(err)
	}
	message := &Message{Pending: true, Accepted: true, InputOrigin: InputOriginUser}
	if err := json.Unmarshal([]byte(payload), &message.Message); err != nil {
		return err
	}
	next, err := PendingUserMessageReplacement(message, content)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(next.Message)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_items SET message_json = ?
		WHERE session_id = ? AND actor_turn_id = ? AND actor_pending = 1 AND actor_accepted = 1 AND input_origin = ? AND id = ?`, string(encoded), sessionID, turnID, InputOriginUser, id)
	if err != nil {
		return classifySQLiteError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrPendingMessageStale
	}
	return classifySQLiteError(tx.Commit())
}
