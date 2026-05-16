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
func (m *MigrationManager) InitializeMigrations(ctx context.Context) error {
	// Create migrations table if it doesn't exist
	err := m.createMigrationsTable(ctx)
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
	_, err := m.db.ExecContext(ctx, `
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
			ErrNewerDatabase, version.Version, maxKnownID, maxAppliedID)
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
func (m *MigrationManager) applyMigration(ctx context.Context, migration *Migration) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		// TODO: handle error
		_ = tx.Rollback()
	}()

	// Execute SQL migration if present
	if migration.UpSQL != "" {
		_, err = tx.ExecContext(ctx, migration.UpSQL)
		if err != nil {
			return fmt.Errorf("failed to execute migration SQL: %w", err)
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
			UpFunc:      migrateMessagesToSessionItems,
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
	}
}

// migrateMessagesToSessionItems migrates data from the messages JSON column to the session_items table
func migrateMessagesToSessionItems(ctx context.Context, db *sql.DB) error {
	slog.InfoContext(ctx, "Starting migration of messages to session_items")

	// Get all sessions that have messages but no items yet
	rows, err := db.QueryContext(ctx, `
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

	slog.InfoContext(ctx, "Found sessions to migrate", "count", len(sessionsToMigrate))

	// Migrate each session
	for _, sess := range sessionsToMigrate {
		if err := migrateSessionMessages(ctx, db, sess.id, sess.messages, ""); err != nil {
			slog.WarnContext(ctx, "Failed to migrate session, skipping", "session_id", sess.id, "error", err)
			continue
		}
	}

	slog.InfoContext(ctx, "Completed migration of messages to session_items")
	return nil
}

// migrateSessionMessages migrates a single session's messages to session_items
func migrateSessionMessages(ctx context.Context, db *sql.DB, sessionID, messagesJSON, parentID string) error {
	var items []Item
	if err := json.Unmarshal([]byte(messagesJSON), &items); err != nil {
		return fmt.Errorf("unmarshaling messages: %w", err)
	}

	// Update parent_id if this is a sub-session
	if parentID != "" {
		_, err := db.ExecContext(ctx, "UPDATE sessions SET parent_id = ? WHERE id = ?", parentID, sessionID)
		if err != nil {
			return fmt.Errorf("updating parent_id: %w", err)
		}
	}

	for position, item := range items {
		if err := migrateItem(ctx, db, sessionID, position, &item); err != nil {
			return fmt.Errorf("migrating item at position %d: %w", position, err)
		}
	}

	return nil
}

// migrateItem migrates a single Item to session_items
func migrateItem(ctx context.Context, db *sql.DB, sessionID string, position int, item *Item) error {
	switch {
	case item.Message != nil:
		// Migrate message
		msgJSON, err := json.Marshal(item.Message.Message)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", err)
		}
		_, err = db.ExecContext(ctx,
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
		err := db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", subSessionID).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Create the sub-session
			subMessagesJSON, jsonErr := json.Marshal(item.SubSession.Messages)
			if jsonErr != nil {
				return fmt.Errorf("marshaling sub-session messages: %w", jsonErr)
			}

			_, execErr := db.ExecContext(ctx,
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
			if migrateErr := migrateSessionMessages(ctx, db, subSessionID, string(subMessagesJSON), sessionID); migrateErr != nil {
				return fmt.Errorf("migrating sub-session messages: %w", migrateErr)
			}
		case err != nil:
			return fmt.Errorf("checking sub-session existence: %w", err)
		default:
			// Sub-session exists, just update parent_id
			_, updateErr := db.ExecContext(ctx, "UPDATE sessions SET parent_id = ? WHERE id = ?", sessionID, subSessionID)
			if updateErr != nil {
				return fmt.Errorf("updating sub-session parent_id: %w", updateErr)
			}
		}

		// Insert subsession reference item
		_, err = db.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, subsession_id)
			 VALUES (?, ?, 'subsession', ?)`,
			sessionID, position, subSessionID)
		if err != nil {
			return fmt.Errorf("inserting subsession item: %w", err)
		}

	case item.Summary != "":
		// Migrate summary
		_, err := db.ExecContext(ctx,
			`INSERT INTO session_items (session_id, position, item_type, summary_text)
			 VALUES (?, ?, 'summary', ?)`,
			sessionID, position, item.Summary)
		if err != nil {
			return fmt.Errorf("inserting summary item: %w", err)
		}
	}

	return nil
}
