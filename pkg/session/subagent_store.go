package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/subagent"
)

// The SQLite session store doubles as the default [subagent.Store]: subagent
// swarm snapshots live in their own table, keyed by the owning session, and
// are cleaned up automatically when the session is deleted (ON DELETE
// CASCADE). The runtime auto-detects this interface on whatever session store
// it was configured with, so embedders that plug a custom database (e.g.
// Postgres) can implement these two methods on their own store — or supply a
// dedicated implementation via runtime.WithSubagentStore.
var _ subagent.DurableStore = (*SQLiteSessionStore)(nil)

// Durability reports SQLite-backed topology persistence.
func (s *SQLiteSessionStore) Durability() subagent.Durability { return subagent.DurabilityDurable }

// SaveTree upserts the swarm snapshot for a session.
func (s *SQLiteSessionStore) SaveTree(ctx context.Context, sessionID string, snapshot subagent.Snapshot) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshaling subagent tree: %w", err)
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
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO subagent_trees (session_id, snapshot) VALUES (?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET snapshot = excluded.snapshot`,
		sessionID, string(data)); err != nil {
		return classifySQLiteError(err)
	}
	return classifySQLiteError(tx.Commit())
}

// LoadTree returns the stored swarm snapshot, or nil when the session has none.
func (s *SQLiteSessionStore) LoadTree(ctx context.Context, sessionID string) (*subagent.Snapshot, error) {
	if sessionID == "" {
		return nil, ErrEmptyID
	}
	var data string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot FROM subagent_trees WHERE session_id = ?`, sessionID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot := &subagent.Snapshot{}
	if err := json.Unmarshal([]byte(data), snapshot); err != nil {
		return nil, fmt.Errorf("unmarshaling subagent tree: %w", err)
	}
	return snapshot, nil
}
