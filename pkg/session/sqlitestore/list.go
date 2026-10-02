package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// ListSessionIDs reads root IDs in the same order as session summaries, without
// migrating, creating, or recovering a database just to populate a picker.
func ListSessionIDs(ctx context.Context, path string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sessions')").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	// Before the sub-session migration every session was a root session.
	var hasParent bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('sessions') WHERE name = 'parent_id')").Scan(&hasParent); err != nil {
		return nil, err
	}
	query := "SELECT id FROM sessions"
	if hasParent {
		query += " WHERE parent_id IS NULL OR parent_id = ''"
	}
	query += " ORDER BY created_at DESC, id ASC"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
