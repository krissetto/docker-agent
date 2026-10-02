package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/version"
)

// Migration represents a database migration
type Migration struct {
	ID          int
	Name        string
	Description string
	UpSQL       string
	DownSQL     string
	AppliedAt   time.Time
	// UpFunc is an optional Go function to run after UpSQL (for data migrations)
	UpFunc func(ctx context.Context, db *sql.DB) error
	// UpTxFunc runs conditional schema changes before recording the migration.
	UpTxFunc func(ctx context.Context, tx *sql.Tx) error
}

// MigrationManager handles database migrations
type MigrationManager struct {
	db         *sql.DB
	migrations []Migration
}

// NewMigrationManager creates a migration manager that runs the production
// migration list against db. Tests that need to drive the manager with a
// synthetic migration list should use NewMigrationManagerWithMigrations
// instead.
func NewMigrationManager(db *sql.DB) *MigrationManager {
	return NewMigrationManagerWithMigrations(db, getAllMigrations())
}

// NewMigrationManagerWithMigrations creates a migration manager bound to the
// supplied migration list. The list is treated as ordered by ID; callers must
// not mutate it after construction. Intended primarily for tests that want to
// exercise specific migration paths without dragging in the full production
// list.
func NewMigrationManagerWithMigrations(db *sql.DB, migrations []Migration) *MigrationManager {
	return &MigrationManager{db: db, migrations: migrations}
}

// InitializeMigrations sets up the migrations table and runs pending migrations
func (m *MigrationManager) InitializeMigrations(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrMigrationFailed, classifySQLiteContextError(ctx, err))
		}
	}()
	// Create migrations table if it doesn't exist
	err = m.createMigrationsTable(ctx)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Check if the database was created by a newer version of the application
	if err := m.checkForUnknownMigrations(ctx); err != nil {
		return err
	}

	// Run all pending migrations
	err = m.RunPendingMigrations(ctx)
	if err != nil {
		return fmt.Errorf("failed to run pending migrations: %w", err)
	}

	return nil
}

// createMigrationsTable creates the migrations tracking table
func (m *MigrationManager) createMigrationsTable(ctx context.Context) error {
	_, err := execSQLiteWrite(ctx, m.db, `
		CREATE TABLE IF NOT EXISTS migrations (
			id INTEGER PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			description TEXT,
			applied_at TEXT NOT NULL
		)
	`)
	return err
}

// checkForUnknownMigrations checks if the database has migrations that this binary
// doesn't know about, which indicates the database was created by a newer version.
// This produces a clear error instead of cryptic SQL failures from schema mismatches.
func (m *MigrationManager) checkForUnknownMigrations(ctx context.Context) error {
	if len(m.migrations) == 0 {
		return nil
	}
	maxKnownID := m.migrations[len(m.migrations)-1].ID

	var maxAppliedID int
	err := m.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM migrations").Scan(&maxAppliedID)
	if err != nil {
		return fmt.Errorf("failed to check applied migrations: %w", err)
	}

	if maxAppliedID > maxKnownID {
		return fmt.Errorf(
			"%w: you are running docker-agent %s which supports migrations up to %d, "+
				"but the session database has migration %d from a newer version; "+
				"please upgrade docker-agent to the latest version",
			ErrNewerDatabase, version.Version, maxKnownID, maxAppliedID,
		)
	}

	return nil
}

// RunPendingMigrations executes all migrations that haven't been applied yet
func (m *MigrationManager) RunPendingMigrations(ctx context.Context) error {
	for _, migration := range m.migrations {
		applied, err := m.isMigrationApplied(ctx, migration.Name)
		if err != nil {
			return fmt.Errorf("failed to check if migration %s is applied: %w", migration.Name, err)
		}

		if !applied {
			err = m.applyMigration(ctx, &migration)
			if err != nil {
				return fmt.Errorf("failed to apply migration %s: %w", migration.Name, err)
			}
		}
	}

	return nil
}

// isMigrationApplied checks if a migration has already been applied
func (m *MigrationManager) isMigrationApplied(ctx context.Context, name string) (bool, error) {
	var count int
	err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM migrations WHERE name = ?", name).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// applyMigration applies a single migration
func (m *MigrationManager) applyMigration(ctx context.Context, migration *Migration) (err error) {
	defer func() { err = classifySQLiteContextError(ctx, err) }()
	tx, err := beginSQLiteWrite(ctx, m.db)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.ErrorContext(ctx, "failed to rollback migration transaction", "error", err)
		}
	}()

	// A deferred transaction must acquire the write lock before reading the
	// migration receipt. This no-op also protects caller-supplied databases
	// whose driver does not support an immediate-transaction DSN option.
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET id = id WHERE 0`); err != nil {
		return err
	}
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM migrations WHERE name = ?`, migration.Name).Scan(&applied); err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}

	// Execute SQL migration if present
	if migration.UpSQL != "" {
		_, err = tx.ExecContext(ctx, migration.UpSQL)
		if err != nil {
			return fmt.Errorf("failed to execute migration SQL: %w", err)
		}
	}

	if migration.UpTxFunc != nil {
		if err := migration.UpTxFunc(ctx, tx); err != nil {
			return fmt.Errorf("failed to execute transactional migration: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx,
		"INSERT INTO migrations (id, name, description, applied_at) VALUES (?, ?, ?, ?)",
		migration.ID, migration.Name, migration.Description, time.Now().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("failed to record migration: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit migration transaction: %w", err)
	}

	// Execute Go function migration if present (after SQL is committed)
	if migration.UpFunc != nil {
		if err := migration.UpFunc(ctx, m.db); err != nil {
			return fmt.Errorf("failed to execute migration function: %w", err)
		}
	}

	return nil
}

// GetAppliedMigrations returns a list of applied migrations
func (m *MigrationManager) GetAppliedMigrations(ctx context.Context) ([]Migration, error) {
	rows, err := m.db.QueryContext(ctx, "SELECT id, name, description, applied_at FROM migrations ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var migrations []Migration
	for rows.Next() {
		var migration Migration
		var appliedAtStr string

		err := rows.Scan(&migration.ID, &migration.Name, &migration.Description, &appliedAtStr)
		if err != nil {
			return nil, err
		}

		migration.AppliedAt, err = time.Parse(time.RFC3339, appliedAtStr)
		if err != nil {
			return nil, err
		}

		migrations = append(migrations, migration)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return migrations, nil
}

// getAllMigrations returns all available migrations in order
func getAllMigrations() []Migration {
	return []Migration{
		{
			ID:          1,
			Name:        "001_add_tools_approved_column",
			Description: "Add tools_approved column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN tools_approved BOOLEAN DEFAULT 0`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN tools_approved`,
		},
		{
			ID:          2,
			Name:        "002_add_usage_column",
			Description: "Add usage column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN input_tokens INTEGER DEFAULT 0`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN input_tokens`,
		},
		{
			ID:          3,
			Name:        "003_add_output_tokens_column",
			Description: "Add output_tokens column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN output_tokens INTEGER DEFAULT 0`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN output_tokens`,
		},
		{
			ID:          4,
			Name:        "004_add_title_column",
			Description: "Add title column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN title TEXT DEFAULT ''`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN title`,
		},
		{
			ID:          5,
			Name:        "005_add_cost_column",
			Description: "Add cost column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN cost REAL DEFAULT 0`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN cost`,
		},
		{
			ID:          6,
			Name:        "006_add_send_user_message_column",
			Description: "Add send_user_message column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN send_user_message BOOLEAN DEFAULT 1`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN send_user_message`,
		},
		{
			ID:          7,
			Name:        "007_add_max_iterations_column",
			Description: "Add max_iterations column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN max_iterations INTEGER DEFAULT 0`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN max_iterations`,
		},
		{
			ID:          8,
			Name:        "008_add_working_dir_column",
			Description: "Add working_dir column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN working_dir TEXT DEFAULT ''`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN working_dir`,
		},
		{
			ID:          9,
			Name:        "009_add_starred_column",
			Description: "Add starred column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN starred BOOLEAN DEFAULT 0`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN starred`,
		},
		{
			ID:          10,
			Name:        "010_add_permissions_column",
			Description: "Add permissions column to sessions table for session-level permission overrides",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN permissions TEXT DEFAULT ''`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN permissions`,
		},
		{
			ID:          11,
			Name:        "011_add_agent_model_overrides_column",
			Description: "Add agent_model_overrides column to sessions table for per-session model switching",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN agent_model_overrides TEXT DEFAULT '{}'`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN agent_model_overrides`,
		},
		{
			ID:          12,
			Name:        "012_add_custom_models_used_column",
			Description: "Add custom_models_used column to sessions table for tracking custom models used in session",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN custom_models_used TEXT DEFAULT '[]'`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN custom_models_used`,
		},
		{
			ID:          13,
			Name:        "013_add_thinking_column",
			Description: "Add thinking column to sessions table for session-level thinking toggle (default enabled)",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN thinking BOOLEAN DEFAULT 1`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN thinking`,
		},
		{
			ID:          14,
			Name:        "014_normalize_session_items",
			Description: "Add session_items table and parent_id column for normalized session storage",
			UpSQL: `
				-- Add parent_id column for sub-session relationship
				ALTER TABLE sessions ADD COLUMN parent_id TEXT REFERENCES sessions(id) ON DELETE CASCADE;

				-- Create index on parent_id for efficient sub-session lookups
				CREATE INDEX IF NOT EXISTS idx_sessions_parent_id ON sessions(parent_id);

				-- Create session_items table for normalized item storage
				CREATE TABLE IF NOT EXISTS session_items (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					session_id TEXT NOT NULL,
					position INTEGER NOT NULL,
					item_type TEXT NOT NULL,
					agent_name TEXT,
					message_json TEXT,
					implicit BOOLEAN DEFAULT 0,
					subsession_id TEXT,
					summary_text TEXT,
					FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
					FOREIGN KEY (subsession_id) REFERENCES sessions(id) ON DELETE SET NULL
				);

				-- Create index for efficient session item lookups
				CREATE INDEX IF NOT EXISTS idx_session_items_session ON session_items(session_id, position);
			`,
			DownSQL: `
				DROP INDEX IF EXISTS idx_session_items_session;
				DROP TABLE IF EXISTS session_items;
				DROP INDEX IF EXISTS idx_sessions_parent_id;
				-- SQLite doesn't support DROP COLUMN directly in older versions
			`,
		},
		{
			ID:          15,
			Name:        "015_migrate_messages_to_session_items",
			Description: "Migrate existing messages JSON data to session_items table",
			UpTxFunc:    migrateMessagesToSessionItems,
		},
		{
			ID:          16,
			Name:        "016_add_branching_columns",
			Description: "Add branch metadata columns for session branching",
			UpSQL: `
				ALTER TABLE sessions ADD COLUMN branch_parent_session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL;
				ALTER TABLE sessions ADD COLUMN branch_parent_position INTEGER;
				ALTER TABLE sessions ADD COLUMN branch_created_at TEXT;
				CREATE INDEX IF NOT EXISTS idx_sessions_branch_parent ON sessions(branch_parent_session_id);
			`,
			DownSQL: `
				DROP INDEX IF EXISTS idx_sessions_branch_parent;
				-- SQLite doesn't support DROP COLUMN directly in older versions
			`,
		},
		{
			ID:          17,
			Name:        "017_add_split_diff_view_column",
			Description: "Add split_diff_view column to sessions table for persisting split diff toggle",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN split_diff_view BOOLEAN`,
		},
		{
			ID:          18,
			Name:        "018_add_session_items_type_index",
			Description: "Add index on session_items(session_id, item_type) to speed up session summary message counts",
			UpSQL:       `CREATE INDEX IF NOT EXISTS idx_session_items_session_type ON session_items(session_id, item_type)`,
		},
		{
			ID:          19,
			Name:        "019_drop_branch_and_split_diff_columns",
			Description: "Drop unused branch metadata columns and split_diff_view column",
			UpSQL: `
				DROP INDEX IF EXISTS idx_sessions_branch_parent;
				ALTER TABLE sessions DROP COLUMN branch_parent_session_id;
				ALTER TABLE sessions DROP COLUMN branch_parent_position;
				ALTER TABLE sessions DROP COLUMN branch_created_at;
				ALTER TABLE sessions DROP COLUMN split_diff_view;
			`,
		},
		{
			ID:          20,
			Name:        "020_drop_messages_column",
			Description: "Drop the legacy messages JSON column now that all data lives in session_items",
			UpSQL:       `ALTER TABLE sessions DROP COLUMN messages`,
		},
		{
			ID:          21,
			Name:        "021_add_first_kept_entry_column",
			Description: "Add first_kept_entry column to session_items for compaction-preserved messages",
			UpSQL:       `ALTER TABLE session_items ADD COLUMN first_kept_entry INTEGER DEFAULT 0`,
		},
		{
			ID:          22,
			Name:        "022_add_instruction_context_column",
			Description: "Persist cache-stable instruction snapshots and chronological updates",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN instruction_context TEXT DEFAULT ''`,
		},
		{
			ID:          23,
			Name:        "023_add_cost_to_session_items",
			Description: "Add cost column to session_items so compaction summary costs survive reload",
			UpSQL:       `ALTER TABLE session_items ADD COLUMN cost REAL NOT NULL DEFAULT 0`,
		},
		{
			ID:          24,
			Name:        "024_add_safety_policy_column",
			Description: "Add safety_policy column to sessions table",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN safety_policy TEXT DEFAULT ''`,
		},
		{
			ID:          25,
			Name:        "025_add_model_usage_to_session_items",
			Description: "Add model and usage_json columns to session_items so compaction summary costs can be attributed per model",
			UpSQL: `
				ALTER TABLE session_items ADD COLUMN model TEXT NOT NULL DEFAULT '';
				ALTER TABLE session_items ADD COLUMN usage_json TEXT NOT NULL DEFAULT '';
			`,
		},
		{
			ID:          26,
			Name:        "026_add_session_attributes_column",
			Description: "Add generic attributes to sessions",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN attributes TEXT DEFAULT '{}'`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN attributes`,
		},
		// Revert PRs must retain this entry: upgraded databases keep the column,
		// and removing it makes older binaries reject them as newer databases.
		{
			ID:          27,
			Name:        "027_add_session_origin_column",
			Description: "Record the protocol surface that created each session",
			UpSQL:       `ALTER TABLE sessions ADD COLUMN origin TEXT NOT NULL DEFAULT 'run'`,
			DownSQL:     `ALTER TABLE sessions DROP COLUMN origin`,
		},
		{
			ID:          28,
			Name:        "028_add_generated_media_manifest_table",
			Description: "Record which workspace files generated-media materialization wrote, keyed by owning session and workspace-relative path",
			// No foreign key to sessions(id): materialization may record a file
			// before the (lazily persisted) session row exists. DeleteSession
			// prunes manifest rows explicitly instead.
			UpSQL: `
				CREATE TABLE IF NOT EXISTS generated_media_manifest (
					session_id TEXT NOT NULL,
					rel_path TEXT NOT NULL,
					mime_type TEXT NOT NULL,
					created_at TEXT NOT NULL,
					PRIMARY KEY (session_id, rel_path)
				)
			`,
			DownSQL: `DROP TABLE IF EXISTS generated_media_manifest`,
		},
		{
			ID:          29,
			Name:        "029_add_generated_media_blob_table",
			Description: "Store generated-media bytes in the session database for portable session rendering",
			UpSQL: `
				CREATE TABLE IF NOT EXISTS generated_media_blobs (
					session_id TEXT NOT NULL,
					rel_path TEXT NOT NULL,
					data BLOB NOT NULL,
					PRIMARY KEY (session_id, rel_path)
				)
			`,
			DownSQL: `DROP TABLE IF EXISTS generated_media_blobs`,
		},
		{
			ID:          30,
			Name:        "030_add_root_kind_to_generated_media_manifest",
			Description: "Add root_kind to generated_media_manifest so user-confirmed out-of-workspace generated files stay manifest-gated alongside workspace-relative ones",
			UpSQL:       `ALTER TABLE generated_media_manifest ADD COLUMN root_kind TEXT NOT NULL DEFAULT 'workspace'`,
		},
		{
			ID:          31,
			Name:        "031_add_actor_pending_message_columns",
			Description: "Persist actor pending input admission and correlation",
			UpSQL: `
				ALTER TABLE session_items ADD COLUMN actor_pending BOOLEAN NOT NULL DEFAULT 0;
				ALTER TABLE session_items ADD COLUMN actor_accepted BOOLEAN NOT NULL DEFAULT 0;
				ALTER TABLE session_items ADD COLUMN actor_turn_id TEXT NOT NULL DEFAULT '';
			`,
		},
		{
			ID:          32,
			Name:        "032_add_subagent_trees_table",
			Description: "Persist async subagent topology snapshots",
			UpSQL: `
				CREATE TABLE IF NOT EXISTS subagent_trees (
					session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
					snapshot TEXT NOT NULL
				);
			`,
			DownSQL: `DROP TABLE IF EXISTS subagent_trees`,
		},
		{
			ID:          33,
			Name:        "033_add_session_todos_table",
			Description: "Persist session todo state",
			UpSQL: `
				CREATE TABLE IF NOT EXISTS session_todos (
					session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
					todos TEXT NOT NULL
				);
			`,
			DownSQL: `DROP TABLE IF EXISTS session_todos`,
		},
		{
			ID:          34,
			Name:        "034_add_child_coordination",
			Description: "Persist child admission, revisioned outcomes and report acknowledgments",
			UpSQL: `
                ALTER TABLE sessions ADD COLUMN execution_settings TEXT NOT NULL DEFAULT '{}';
                ALTER TABLE session_items ADD COLUMN actor_input_mode TEXT NOT NULL DEFAULT '';
                ALTER TABLE session_items ADD COLUMN write_id TEXT NOT NULL DEFAULT '';
                ALTER TABLE session_items ADD COLUMN write_hash TEXT NOT NULL DEFAULT '';
                CREATE UNIQUE INDEX session_items_write_id ON session_items(session_id, write_id) WHERE write_id != '';
                CREATE TABLE child_records (
                    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
                    root_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
                    parent_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
                    revision INTEGER NOT NULL,
                    record TEXT NOT NULL
                );
                CREATE INDEX child_records_root ON child_records(root_session_id);
                CREATE TABLE child_reports (
                    id TEXT PRIMARY KEY,
                    parent_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
                    child_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
                    turn_id TEXT NOT NULL,
                    revision INTEGER NOT NULL,
                    content TEXT NOT NULL,
                    message_id INTEGER
                );
                CREATE INDEX child_reports_pending ON child_reports(parent_session_id) WHERE message_id IS NULL;
            `,
			DownSQL: `DROP TABLE IF EXISTS child_reports; DROP TABLE IF EXISTS child_records;`,
		},
		{
			ID:          35,
			Name:        "035_complete_child_coordination_schema",
			Description: "Complete execution settings and keyed append columns on early coordination databases",
			UpTxFunc:    completeChildCoordinationSchema,
		},
		{
			ID:          36,
			Name:        "036_add_input_provenance",
			Description: "Persist trusted input origin and sender attribution",
			UpSQL: `
                ALTER TABLE session_items ADD COLUMN input_origin TEXT NOT NULL DEFAULT '';
                ALTER TABLE session_items ADD COLUMN sender_id TEXT NOT NULL DEFAULT '';
                ALTER TABLE session_items ADD COLUMN sender_name TEXT NOT NULL DEFAULT '';
            `,
			DownSQL: `ALTER TABLE session_items DROP COLUMN sender_name; ALTER TABLE session_items DROP COLUMN sender_id; ALTER TABLE session_items DROP COLUMN input_origin;`,
		},
		{ID: 37, Name: "037_session_input_batches", Description: "Atomic input batch idempotency receipts", UpSQL: `CREATE TABLE session_input_batches (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, request_id TEXT NOT NULL, content_hash TEXT NOT NULL, item_ids TEXT NOT NULL, PRIMARY KEY(session_id,request_id));`, DownSQL: `DROP TABLE session_input_batches;`},
		{ID: 38, Name: "038_child_report_outcome", Description: "Persist historical child turn outcomes", UpSQL: `ALTER TABLE session_items ADD COLUMN report_outcome TEXT NOT NULL DEFAULT ''; ALTER TABLE child_reports ADD COLUMN report_outcome TEXT NOT NULL DEFAULT '';`, DownSQL: `ALTER TABLE child_reports DROP COLUMN report_outcome; ALTER TABLE session_items DROP COLUMN report_outcome;`},
	}
}

// migrateMessagesToSessionItems migrates data from the messages JSON column to the session_items table
func migrateMessagesToSessionItems(ctx context.Context, tx *sql.Tx) error {
	slog.InfoContext(ctx, "Starting migration of messages to session_items")

	// Get all sessions that have messages but no items yet
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.messages 
		FROM sessions s 
		WHERE s.messages IS NOT NULL 
		  AND s.messages != '' 
		  AND s.messages != '[]'
		  AND NOT EXISTS (SELECT 1 FROM session_items si WHERE si.session_id = s.id)
	`)
	if err != nil {
		return fmt.Errorf("querying sessions: %w", err)
	}
	defer rows.Close()

	var sessionsToMigrate []struct {
		id       string
		messages string
	}

	for rows.Next() {
		var id, messages string
		if err := rows.Scan(&id, &messages); err != nil {
			return fmt.Errorf("scanning session: %w", err)
		}
		sessionsToMigrate = append(sessionsToMigrate, struct {
			id       string
			messages string
		}{id, messages})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating sessions: %w", err)
	}

	// Close the result set before issuing writes on the same transaction.
	if err := rows.Close(); err != nil {
		return err
	}

	slog.InfoContext(ctx, "Found sessions to migrate", "count", len(sessionsToMigrate))

	// Migrate each session
	for _, sess := range sessionsToMigrate {
		if err := migrateSessionMessages(ctx, tx, sess.id, sess.messages, ""); err != nil {
			return fmt.Errorf("migrating session %s: %w", sess.id, err)
		}
	}

	slog.InfoContext(ctx, "Completed migration of messages to session_items")
	return nil
}

// migrateSessionMessages migrates a single session's messages to session_items
func migrateSessionMessages(ctx context.Context, tx *sql.Tx, sessionID, messagesJSON, parentID string) error {
	var items []Item
	if err := json.Unmarshal([]byte(messagesJSON), &items); err != nil {
		return fmt.Errorf("unmarshaling messages: %w", err)
	}

	// Update parent_id if this is a sub-session
	if parentID != "" {
		_, err := tx.ExecContext(ctx, "UPDATE sessions SET parent_id = ? WHERE id = ?", parentID, sessionID)
		if err != nil {
			return fmt.Errorf("updating parent_id: %w", err)
		}
	}

	for position, item := range items {
		if err := migrateItem(ctx, tx, sessionID, position, &item); err != nil {
			return fmt.Errorf("migrating item at position %d: %w", position, err)
		}
	}

	return nil
}

// migrateItem migrates a single Item to session_items
func migrateItem(ctx context.Context, tx *sql.Tx, sessionID string, position int, item *Item) error {
	switch {
	case item.Message != nil:
		// Migrate message
		msgJSON, err := json.Marshal(item.Message.Message)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, agent_name, message_json, implicit)
			 VALUES (?, ?, 'message', ?, ?, ?)`,
			sessionID, position, item.Message.AgentName, string(msgJSON), item.Message.Implicit)
		if err != nil {
			return fmt.Errorf("inserting message item: %w", err)
		}

	case item.SubSession != nil:
		// Create sub-session and link to parent
		subSessionID := item.SubSession.ID
		if subSessionID == "" {
			subSessionID = uuid.New().String()
		}

		// Check if sub-session already exists
		var exists int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", subSessionID).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Create the sub-session
			subMessagesJSON, jsonErr := json.Marshal(item.SubSession.Messages)
			if jsonErr != nil {
				return fmt.Errorf("marshaling sub-session messages: %w", jsonErr)
			}

			_, execErr := tx.ExecContext(ctx,
				`INSERT INTO sessions (id, messages, tools_approved, input_tokens, output_tokens, title, cost, 
				 send_user_message, max_iterations, working_dir, created_at, starred, permissions, 
				 agent_model_overrides, custom_models_used, parent_id)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				subSessionID, string(subMessagesJSON), item.SubSession.ToolsApproved,
				item.SubSession.InputTokens, item.SubSession.OutputTokens, item.SubSession.Title,
				item.SubSession.Cost, item.SubSession.SendUserMessage, item.SubSession.MaxIterations,
				item.SubSession.WorkingDir, item.SubSession.CreatedAt.Format(time.RFC3339),
				item.SubSession.Starred, "", "{}", "[]", sessionID)
			if execErr != nil {
				return fmt.Errorf("inserting sub-session: %w", execErr)
			}

			// Recursively migrate sub-session messages
			if migrateErr := migrateSessionMessages(ctx, tx, subSessionID, string(subMessagesJSON), sessionID); migrateErr != nil {
				return fmt.Errorf("migrating sub-session messages: %w", migrateErr)
			}
		case err != nil:
			return fmt.Errorf("checking sub-session existence: %w", err)
		default:
			// Sub-session exists, just update parent_id
			_, updateErr := tx.ExecContext(ctx, "UPDATE sessions SET parent_id = ? WHERE id = ?", sessionID, subSessionID)
			if updateErr != nil {
				return fmt.Errorf("updating sub-session parent_id: %w", updateErr)
			}
		}

		// Insert subsession reference item
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, subsession_id)
			 VALUES (?, ?, 'subsession', ?)`,
			sessionID, position, subSessionID)
		if err != nil {
			return fmt.Errorf("inserting subsession item: %w", err)
		}

	case item.Summary != "":
		// Migrate summary
		_, err := tx.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, summary_text)
			 VALUES (?, ?, 'summary', ?)`,
			sessionID, position, item.Summary)
		if err != nil {
			return fmt.Errorf("inserting summary item: %w", err)
		}
	}

	return nil
}
