package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var _ TodoStore = (*SQLiteSessionStore)(nil)

func (s *SQLiteSessionStore) LoadTodos(ctx context.Context, sessionID string) ([]Todo, error) {
	if sessionID == "" {
		return nil, ErrEmptyID
	}
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT todos FROM session_todos WHERE session_id = ?`, sessionID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return []Todo{}, nil
	}
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	var todos []Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return nil, fmt.Errorf("unmarshaling session todos: %w", err)
	}
	if todos == nil {
		todos = []Todo{}
	}
	return todos, nil
}

func (s *SQLiteSessionStore) SaveTodos(ctx context.Context, sessionID string, todos []Todo) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return fmt.Errorf("marshaling session todos: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return classifySQLiteError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_todos (session_id, todos) VALUES (?, ?)
		ON CONFLICT(session_id) DO UPDATE SET todos = excluded.todos`, sessionID, string(data)); err != nil {
		return classifySQLiteError(err)
	}
	return classifySQLiteError(tx.Commit())
}

func (s *SQLiteSessionStore) MutateTodos(ctx context.Context, sessionID string, fn func([]Todo) ([]Todo, error)) ([]Todo, error) {
	if sessionID == "" {
		return nil, ErrEmptyID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var data string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT todos FROM session_todos WHERE session_id = ?), '[]') FROM sessions WHERE id = ?`, sessionID, sessionID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, classifySQLiteError(err)
	}
	var items []Todo
	if err := json.Unmarshal([]byte(data), &items); err != nil {
		return nil, err
	}
	items, err = fn(items)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_todos (session_id, todos) VALUES (?, ?) ON CONFLICT(session_id) DO UPDATE SET todos=excluded.todos`, sessionID, string(encoded)); err != nil {
		return nil, classifySQLiteError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, classifySQLiteError(err)
	}
	return items, nil
}
