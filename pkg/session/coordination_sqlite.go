package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (s *SQLiteSessionStore) AdmitChild(ctx context.Context, a ChildAdmission) (err error) {
	defer func() { err = classifySQLiteError(err) }()
	child, record, err := prepareAdmission(a)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	old, err := loadChildRecord(ctx, tx, child.ID)
	if err == nil {
		if old.RootSessionID == record.RootSessionID && old.ParentSessionID == record.ParentSessionID && old.Node.ID == record.Node.ID && old.Node.Agent == record.Node.Agent && old.Node.Parent == record.Node.Parent {
			return nil
		}
		return ErrAlreadyExists
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	for _, id := range []string{record.RootSessionID, record.ParentSessionID} {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
	}
	var collision int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM child_records WHERE root_session_id = ? AND json_extract(record, '$.Node.id') = ?`, record.RootSessionID, record.Node.ID).Scan(&collision)
	if err == nil {
		return ErrAlreadyExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	lookup := func(id string) (*Session, error) {
		sess, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionSelectColumns+" FROM sessions WHERE id = ?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return sess, err
	}
	existing, err := lookup(child.ID)
	switch {
	case err == nil:
		if err := validateChildAdoption(existing, child, record, lookup); err != nil {
			return err
		}
	case errors.Is(err, ErrNotFound):
		if err := validateNewChild(child); err != nil {
			return err
		}
		if err := s.upsertSessionRowTx(ctx, tx, child); err != nil {
			return err
		}
		for position, item := range child.Messages {
			if err := s.addItemTx(ctx, tx, child.ID, position, item); err != nil {
				return err
			}
		}
	default:
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_items(session_id, position, item_type, subsession_id)
      SELECT ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?), 'subsession', ?
      WHERE NOT EXISTS (SELECT 1 FROM session_items WHERE session_id = ? AND item_type = 'subsession' AND subsession_id = ?)`, record.ParentSessionID, record.ParentSessionID, child.ID, record.ParentSessionID, child.ID); err != nil {
		return err
	}

	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO child_records(session_id, root_session_id, parent_session_id, revision, record) VALUES (?, ?, ?, ?, ?)`, child.ID, record.RootSessionID, record.ParentSessionID, record.Revision, string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

func loadChildRecord(ctx context.Context, q querier, id string) (ChildRecord, error) {
	var record ChildRecord
	var data string
	err := q.QueryRowContext(ctx, `SELECT record FROM child_records WHERE session_id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	err = json.Unmarshal([]byte(data), &record)
	return record, err
}

func (s *SQLiteSessionStore) CommitChild(ctx context.Context, c ChildCommit) (err error) {
	defer func() { err = classifySQLiteError(err) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	old, err := loadChildRecord(ctx, tx, c.Record.Node.SessionID)
	if err != nil {
		return err
	}
	record, err := prepareCommit(c, old)
	if err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE child_records SET revision = ?, record = ? WHERE session_id = ? AND revision = ?`, record.Revision, string(data), record.Node.SessionID, c.ExpectedRevision)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrRevisionConflict
	}
	for _, report := range c.Reports {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM child_reports WHERE id = ?`, report.ID).Scan(&exists)
		if err == nil {
			return ErrAlreadyExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO child_reports(id, parent_session_id, child_session_id, turn_id, revision, content) VALUES (?, ?, ?, ?, ?, ?)`, report.ID, report.ParentSessionID, report.ChildSessionID, report.TurnID, record.Revision, report.Content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteSessionStore) LoadChildren(ctx context.Context, root string) ([]ChildRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM child_records WHERE root_session_id = ? ORDER BY rowid`, root)
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	defer rows.Close()
	var records []ChildRecord
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var record ChildRecord
		if err := json.Unmarshal([]byte(data), &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, classifySQLiteError(rows.Err())
}

func (s *SQLiteSessionStore) PendingReports(ctx context.Context, parent string) ([]ChildReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, parent_session_id, child_session_id, turn_id, revision, content FROM child_reports WHERE parent_session_id = ? AND message_id IS NULL ORDER BY rowid`, parent)
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	defer rows.Close()
	var reports []ChildReport
	for rows.Next() {
		var report ChildReport
		if err := rows.Scan(&report.ID, &report.ParentSessionID, &report.ChildSessionID, &report.TurnID, &report.Revision, &report.Content); err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, classifySQLiteError(rows.Err())
}

func (s *SQLiteSessionStore) AcceptReport(ctx context.Context, parent, id string, message *Message) (acceptance ReportAcceptance, err error) {
	defer func() { err = classifySQLiteError(err) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReportAcceptance{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var accepted sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT message_id FROM child_reports WHERE id = ? AND parent_session_id = ?`, id, parent).Scan(&accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportAcceptance{}, ErrNotFound
	}
	if err != nil {
		return ReportAcceptance{}, err
	}
	if accepted.Valid {
		result := ReportAcceptance{MessageID: accepted.Int64}
		var stored Message
		var data string
		err := tx.QueryRowContext(ctx, `SELECT id, COALESCE(agent_name, ''), message_json, implicit, COALESCE(actor_pending, 0), COALESCE(actor_accepted, 0), COALESCE(actor_turn_id, ''), COALESCE(actor_input_mode, ''), input_origin, sender_id, sender_name
			FROM session_items WHERE session_id = ? AND id = ? AND item_type = 'message'`, parent, accepted.Int64).Scan(
			&stored.ID, &stored.AgentName, &data, &stored.Implicit, &stored.Pending, &stored.Accepted, &stored.TurnID, &stored.InputMode, &stored.InputOrigin, &stored.SenderID, &stored.SenderName)
		if errors.Is(err, sql.ErrNoRows) {
			return result, nil
		}
		if err != nil {
			return ReportAcceptance{}, err
		}
		if err := json.Unmarshal([]byte(data), &stored.Message); err != nil {
			return ReportAcceptance{}, err
		}
		result.Message = &stored
		return result, nil
	}
	if message == nil {
		return ReportAcceptance{}, nil
	}
	if message.TurnID == "" {
		return ReportAcceptance{}, errors.New("report input requires a turn ID")
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, parent).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ReportAcceptance{}, ErrNotFound
	} else if err != nil {
		return ReportAcceptance{}, err
	}
	queued := cloneMessage(message)
	queued.Pending, queued.Accepted = true, true
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?`, parent).Scan(&position); err != nil {
		return ReportAcceptance{}, err
	}
	if err := s.addItemTx(ctx, tx, parent, position, Item{Message: queued}); err != nil {
		return ReportAcceptance{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&queued.ID); err != nil {
		return ReportAcceptance{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE child_reports SET message_id = ?, content = '' WHERE id = ? AND parent_session_id = ?`, queued.ID, id, parent); err != nil {
		return ReportAcceptance{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportAcceptance{}, err
	}
	return ReportAcceptance{MessageID: queued.ID, Message: queued, Created: true}, nil
}
