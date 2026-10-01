package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/subagent"
)

// ChildReactivationStore is the only transition out of a stopped child. It
// atomically retires obsolete accepted inputs and accepts one fresh user turn.
// Generic child commits deliberately cannot perform this transition.
type ChildReactivationStore interface {
	ReactivateChildWithInput(context.Context, ChildRecord, *Message) (ChildRecord, *Message, error)
}

func prepareReactivation(expected, old ChildRecord, input *Message) (ChildRecord, error) {
	if expected.Revision != old.Revision || old.Revision == ^uint64(0) {
		return ChildRecord{}, ErrRevisionConflict
	}
	if !sameChildIdentity(expected, old) || old.Node.State != subagent.NodeStopped {
		return ChildRecord{}, ErrRevisionConflict
	}
	if input == nil || input.TurnID == "" || input.InputOrigin != InputOriginUser || input.Message.Role != chat.MessageRoleUser || input.InputMode != "turn" || (strings.TrimSpace(input.Message.Content) == "" && len(input.Message.MultiContent) == 0) {
		return ChildRecord{}, errors.New("reactivation requires fresh user input")
	}
	old.Revision++
	old.Node.State, old.Node.NeedsAttention, old.Node.WaitingOn = subagent.NodeIdle, false, ""
	old.ReactivationTurnID = input.TurnID
	return old, nil
}

func (s *SQLiteSessionStore) ReactivateChildWithInput(ctx context.Context, expected ChildRecord, input *Message) (record ChildRecord, accepted *Message, err error) {
	defer func() { err = classifySQLiteContextError(ctx, err) }()
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return record, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	old, err := loadChildRecord(ctx, tx, expected.Node.SessionID)
	if err != nil {
		return record, nil, err
	}
	record, err = prepareReactivation(expected, old, input)
	if err != nil {
		return record, nil, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, record.Node.SessionID).Scan(&exists); err != nil {
		return record, nil, err
	}
	if exists != 1 {
		return record, nil, ErrNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_items WHERE session_id = ? AND actor_turn_id = ?`, record.Node.SessionID, input.TurnID).Scan(&exists); err != nil {
		return record, nil, err
	}
	if exists != 0 {
		return record, nil, ErrAlreadyExists
	}
	data, err := json.Marshal(record)
	if err != nil {
		return record, nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE child_records SET revision = ?, record = ? WHERE session_id = ? AND revision = ?`, record.Revision, string(data), record.Node.SessionID, expected.Revision)
	if err != nil {
		return record, nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return record, nil, err
	}
	if changed != 1 {
		return record, nil, ErrRevisionConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_items SET actor_accepted = 0 WHERE session_id = ? AND actor_pending = 1`, record.Node.SessionID); err != nil {
		return record, nil, err
	}
	// Unacknowledged reports predate the explicit restart and must not wake it.
	if _, err := tx.ExecContext(ctx, `DELETE FROM child_reports WHERE parent_session_id = ? AND message_id IS NULL`, record.Node.SessionID); err != nil {
		return record, nil, err
	}
	accepted = cloneMessage(input)
	accepted.Pending, accepted.Accepted = true, true
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?`, record.Node.SessionID).Scan(&position); err != nil {
		return record, nil, err
	}
	if err := s.addItemTx(ctx, tx, record.Node.SessionID, position, Item{Message: accepted}); err != nil {
		return record, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&accepted.ID); err != nil {
		return record, nil, err
	}
	if err := tx.Commit(); err != nil {
		return record, nil, err
	}
	return record, accepted, nil
}

func (s *InMemorySessionStore) ReactivateChildWithInput(ctx context.Context, expected ChildRecord, input *Message) (ChildRecord, *Message, error) {
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ChildRecord{}, nil, err
	}
	old, ok := s.children[expected.Node.SessionID]
	if !ok {
		return ChildRecord{}, nil, ErrNotFound
	}
	record, err := prepareReactivation(expected, old, input)
	if err != nil {
		return ChildRecord{}, nil, err
	}
	child, ok := s.sessions.Load(record.Node.SessionID)
	if !ok {
		return ChildRecord{}, nil, ErrNotFound
	}
	child = child.Clone()
	for _, item := range child.Messages {
		if item.Message == nil {
			continue
		}
		if item.Message.TurnID == input.TurnID {
			return ChildRecord{}, nil, ErrAlreadyExists
		}
		if item.Message.Pending {
			item.Message.Accepted = false
		}
	}
	accepted := cloneMessage(input)
	accepted.ID = s.messageID.Add(1)
	accepted.Pending, accepted.Accepted = true, true
	child.AddMessage(accepted)
	if err := ctx.Err(); err != nil {
		return ChildRecord{}, nil, err
	}
	s.sessions.Store(child.ID, child)
	for id, report := range s.reports {
		if report.report.ParentSessionID == child.ID && report.messageID == 0 {
			delete(s.reports, id)
		}
	}
	s.children[child.ID] = record
	return record, cloneMessage(accepted), nil
}
