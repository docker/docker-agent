package sqliteutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// OpenDB opens a SQLite database with recommended pragmas for concurrency and foreign key support.
// It configures the connection pool for serialized writes (MaxOpenConns=1).
func OpenDB(path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create database directory %q: %w", dir, err)
	}

	// Add query parameters for better concurrency handling and data integrity
	// _pragma=busy_timeout(5000): Wait up to 5 seconds if database is locked
	// _pragma=journal_mode(WAL): Enable Write-Ahead Logging for better concurrent access
	// _pragma=foreign_keys(1): Enable foreign key constraints (critical for ON DELETE CASCADE)
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		if IsCantOpenError(err) {
			return nil, DiagnoseDBOpenError(path, err)
		}
		return nil, err
	}

	// Configure connection pool to serialize writes (SQLite limitation)
	// This prevents "database is locked" errors from concurrent writes
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Verify connection works (this will trigger file creation/open)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		if IsCantOpenError(err) {
			return nil, DiagnoseDBOpenError(path, err)
		}
		return nil, err
	}

	return db, nil
}

// IsCantOpenError checks if the error is a SQLite CANTOPEN error (code 14).
func IsCantOpenError(err error) bool {
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		return sqliteErr.Code() == sqlite3.SQLITE_CANTOPEN
	}
	return false
}

// DiagnoseDBOpenError provides a more helpful error message when SQLite
// fails to open/create a database file.
func DiagnoseDBOpenError(path string, originalErr error) error {
	dir := filepath.Dir(path)

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cannot create database at %q: directory %q does not exist", path, dir)
		}
		return fmt.Errorf("cannot create database at %q: %w", path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("cannot create database at %q: %q is not a directory", path, dir)
	}

	return fmt.Errorf("cannot create database at %q: permission denied or file cannot be created in %q (original error: %w)", path, dir, originalErr)
}

// CheckpointAndClose runs a final WAL checkpoint and closes the database.
// The TRUNCATE checkpoint folds the -wal file back into the main database
// so it isn't left behind on disk after shutdown. A checkpoint failure is
// logged but does not prevent the close.
func CheckpointAndClose(db *sql.DB) error {
	if _, err := db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.Warn("Failed to checkpoint WAL before close", "error", err)
	}
	return db.Close()
}
