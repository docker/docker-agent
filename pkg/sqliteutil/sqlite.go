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
func OpenDB(ctx context.Context, path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create database directory %q: %w", dir, err)
	}

	// Add query parameters for better concurrency handling and data integrity
	// _pragma=busy_timeout(5000): Wait up to 5 seconds if database is locked
	// _pragma=journal_mode(...): WAL outside sandboxes, DELETE inside (see journalMode)
	// _pragma=foreign_keys(1): Enable foreign key constraints (critical for ON DELETE CASCADE)
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(" + journalMode() + ")&_pragma=foreign_keys(1)"

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
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		if IsCantOpenError(err) {
			return nil, DiagnoseDBOpenError(path, err)
		}
		return nil, err
	}

	return db, nil
}

// journalMode picks the SQLite journal mode. WAL is preferred, but it relies
// on a shared-memory index (the -shm file) mmap'd by every connection. Inside
// a Docker sandbox the data dir is bind-mounted from the host (virtiofs),
// where that mapping is not coherent across the VM boundary and WAL corrupts
// the database. DELETE mode uses only ordinary file locks, which the mount
// forwards correctly; it also folds any leftover -wal file back into the main
// database on open.
func journalMode() string {
	// Mirrors environment.InSandbox; checked directly to keep sqliteutil
	// dependency-free.
	if os.Getenv("SANDBOX_VM_ID") != "" {
		return "DELETE"
	}
	return "WAL"
}

// IsCantOpenError checks if the error is a SQLite CANTOPEN error (code 14).
func IsCantOpenError(err error) bool {
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		return sqliteErr.Code() == sqlite3.SQLITE_CANTOPEN
	}
	return false
}

// IsTransientError reports whether err is a temporary condition that a retry
// (not a schema fix) would resolve: a canceled/expired context, or a SQLite
// BUSY/LOCKED error from a concurrent writer. The primary result code is
// compared after masking off extended-code bits (e.g. SQLITE_BUSY_SNAPSHOT).
func IsTransientError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
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
func CheckpointAndClose(ctx context.Context, db *sql.DB) error {
	ctx = context.WithoutCancel(ctx)
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.WarnContext(ctx, "Failed to checkpoint WAL before close", "error", err)
	}
	return db.Close()
}
