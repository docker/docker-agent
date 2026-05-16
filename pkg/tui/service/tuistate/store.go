// Package tuistate provides persistent TUI state storage (tabs, recent/favorite directories).
package tuistate

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// Store manages persistent TUI state in a SQLite database.
type Store struct {
	db *sql.DB
}

// New creates a new TUI state store, initializing the database if needed.
func New() (*Store, error) {
	dbPath := filepath.Join(paths.GetDataDir(), "tui_state.db")
	db, err := sqliteutil.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening TUI state store: %w", err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating TUI state store: %w", err)
	}

	return store, nil
}

// migrate runs database migrations.
func (s *Store) migrate() error {
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS tabs (
			session_id TEXT PRIMARY KEY,
			working_dir TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_active_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS active_tab (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			session_id TEXT NOT NULL
		);
		
		CREATE TABLE IF NOT EXISTS recent_dirs (
			path TEXT PRIMARY KEY,
			used_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS favorite_dirs (
			path TEXT PRIMARY KEY,
			added_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return err
	}

	// Add sidebar_collapsed column if it doesn't exist (migration for existing databases).
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE tabs ADD COLUMN sidebar_collapsed BOOLEAN NOT NULL DEFAULT 0`)

	// Add position column for explicit tab ordering.
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE tabs ADD COLUMN position INTEGER NOT NULL DEFAULT 0`)

	// Backfill position for databases that predate the column: assign positions
	// based on existing created_at order so the visible order is preserved.
	// Only runs when all positions are 0 (i.e. column was just added).
	var maxPos int
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) FROM tabs`).Scan(&maxPos)
	if maxPos == 0 {
		_, _ = s.db.ExecContext(ctx, `
			UPDATE tabs SET position = (
				SELECT COUNT(*) FROM tabs t2 WHERE t2.created_at < tabs.created_at
			)
		`)
	}

	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// AddTab adds a new tab to the store, placing it after all existing tabs.
func (s *Store) AddTab(ctx context.Context, sessionID, workingDir string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tabs (session_id, working_dir, created_at, last_active_at, position)
		VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, (SELECT COALESCE(MAX(position), -1) + 1 FROM tabs))
	`, sessionID, workingDir)
	if err != nil {
		return fmt.Errorf("adding tab: %w", err)
	}

	// Also track the working directory as recently used (skip empty paths)
	if workingDir != "" {
		_, err = s.db.ExecContext(ctx, `
			INSERT OR REPLACE INTO recent_dirs (path, used_at)
			VALUES (?, CURRENT_TIMESTAMP)
		`, workingDir)
		return err
	}
	return nil
}

// RemoveTab removes a tab from the store and compacts the remaining positions.
func (s *Store) RemoveTab(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tabs WHERE session_id = ?`, sessionID)
	if err != nil {
		return err
	}
	// Compact positions so they stay contiguous (0, 1, 2, …).
	_, _ = s.db.ExecContext(ctx, `
		UPDATE tabs SET position = (
			SELECT COUNT(*) FROM tabs t2 WHERE t2.position < tabs.position
		)
	`)
	return nil
}

// UpdateTabSessionID replaces the session ID for a tab entry.
// Used after restoring a session: the tab store initially holds an ephemeral
// runner ID, which must be updated to the actual session store ID so that
// the session can be found on the next restart.
func (s *Store) UpdateTabSessionID(ctx context.Context, oldID, newID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tabs SET session_id = ? WHERE session_id = ?`, newID, oldID)
	return err
}

// ReorderTab persists a new tab order. sessionIDs must contain every tab's
// persisted session ID in the desired order; positions are assigned 0..N-1.
func (s *Store) ReorderTab(ctx context.Context, sessionIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting reorder transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	stmt, err := tx.PrepareContext(ctx, `UPDATE tabs SET position = ? WHERE session_id = ?`)
	if err != nil {
		return fmt.Errorf("preparing reorder statement: %w", err)
	}
	defer stmt.Close()

	for i, id := range sessionIDs {
		if _, err := stmt.ExecContext(ctx, i, id); err != nil {
			return fmt.Errorf("updating position for %s: %w", id, err)
		}
	}

	return tx.Commit()
}

// UpdateTabWorkingDir updates the stored working directory for the given session.
func (s *Store) UpdateTabWorkingDir(ctx context.Context, sessionID, workingDir string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tabs SET working_dir = ? WHERE session_id = ?`, workingDir, sessionID)
	return err
}

// SetActiveTab sets the currently active tab.
func (s *Store) SetActiveTab(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO active_tab (id, session_id)
		VALUES (1, ?)
	`, sessionID)
	return err
}

// GetRecentDirs returns the most recently used directories.
func (s *Store) GetRecentDirs(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path FROM recent_dirs
		ORDER BY used_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dirs []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		dirs = append(dirs, path)
	}
	return dirs, rows.Err()
}

// TabEntry represents a persisted tab.
type TabEntry struct {
	SessionID        string
	WorkingDir       string
	SidebarCollapsed bool
}

// GetTabs returns all persisted tabs in position order, along with the active tab's session ID.
func (s *Store) GetTabs(ctx context.Context) ([]TabEntry, string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, working_dir, sidebar_collapsed FROM tabs
		ORDER BY position ASC
	`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var tabs []TabEntry
	for rows.Next() {
		var t TabEntry
		if err := rows.Scan(&t.SessionID, &t.WorkingDir, &t.SidebarCollapsed); err != nil {
			return nil, "", err
		}
		tabs = append(tabs, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var activeID string
	_ = s.db.QueryRowContext(ctx, `SELECT session_id FROM active_tab WHERE id = 1`).Scan(&activeID)

	return tabs, activeID, nil
}

// ToggleSidebarCollapsed inverts the sidebar collapsed state for a tab.
func (s *Store) ToggleSidebarCollapsed(ctx context.Context, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tabs SET sidebar_collapsed = NOT sidebar_collapsed WHERE session_id = ?`, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no tab found with session_id %q", sessionID)
	}
	return nil
}

// ClearTabs removes all tabs from the store. Used when starting fresh (no tabs to restore).
func (s *Store) ClearTabs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tabs`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM active_tab`)
	return err
}

// AddFavoriteDir adds a directory to the favorites list.
func (s *Store) AddFavoriteDir(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO favorite_dirs (path, added_at)
		VALUES (?, CURRENT_TIMESTAMP)
	`, path)
	return err
}

// RemoveFavoriteDir removes a directory from the favorites list.
func (s *Store) RemoveFavoriteDir(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM favorite_dirs WHERE path = ?`, path)
	return err
}

// GetFavoriteDirs returns all favorite directories, ordered by most recently added.
func (s *Store) GetFavoriteDirs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path FROM favorite_dirs
		ORDER BY added_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dirs []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		dirs = append(dirs, path)
	}
	return dirs, rows.Err()
}

// IsFavoriteDir checks if a directory is in the favorites list.
func (s *Store) IsFavoriteDir(ctx context.Context, path string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM favorite_dirs WHERE path = ?`, path).Scan(&count)
	return count > 0, err
}

// ToggleFavoriteDir adds or removes a directory from favorites. Returns the new state (true = now favorite).
func (s *Store) ToggleFavoriteDir(ctx context.Context, path string) (bool, error) {
	isFav, err := s.IsFavoriteDir(ctx, path)
	if err != nil {
		return false, err
	}
	if isFav {
		return false, s.RemoveFavoriteDir(ctx, path)
	}
	return true, s.AddFavoriteDir(ctx, path)
}
