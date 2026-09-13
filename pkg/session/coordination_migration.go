package session

import (
	"context"
	"database/sql"
	"fmt"
)

// Early development builds recorded migration 34 before its final columns were
// present. Repair by schema inspection, not by rerunning an applied migration.
func completeChildCoordinationSchema(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []struct {
		name    string
		columns []string
	}{
		{"sessions", []string{"execution_settings"}},
		{"session_items", []string{"actor_input_mode", "write_id", "write_hash"}},
	} {
		present := map[string]bool{}
		// Close schema-inspection rows before issuing SQLite schema writes.
		err := func() (err error) {
			rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table.name+")")
			if err != nil {
				return err
			}
			defer func() {
				if closeErr := rows.Close(); err == nil {
					err = closeErr
				}
			}()
			for rows.Next() {
				var cid, notNull, primaryKey int
				var name, columnType string
				var defaultValue sql.NullString
				if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
					return err
				}
				present[name] = true
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, column := range table.columns {
			if present[column] {
				continue
			}
			defaultValue := "''"
			if column == "execution_settings" {
				defaultValue = "'{}'"
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s TEXT NOT NULL DEFAULT %s", table.name, column, defaultValue)); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS session_items_write_id ON session_items(session_id, write_id) WHERE write_id != ''`)
	return err
}
