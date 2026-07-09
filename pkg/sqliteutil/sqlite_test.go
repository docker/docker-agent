package sqliteutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queryJournalMode opens path via OpenDB and reports the effective journal mode.
func queryJournalMode(t *testing.T, path string) string {
	t.Helper()

	db, err := OpenDB(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()

	var mode string
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
	return mode
}

func TestOpenDB_UsesWALOnHost(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "")

	mode := queryJournalMode(t, filepath.Join(t.TempDir(), "host.db"))
	assert.Equal(t, "wal", mode)
}

// WAL is unsafe on the bind-mounted data dir inside a Docker sandbox (its
// shared-memory index is not coherent across the VM boundary and corrupts the
// database), so OpenDB must fall back to the DELETE journal there.
func TestOpenDB_UsesDeleteJournalInSandbox(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "vm-1")

	mode := queryJournalMode(t, filepath.Join(t.TempDir(), "sandbox.db"))
	assert.Equal(t, "delete", mode)
}

// A database created in WAL mode on the host must convert cleanly when
// reopened inside a sandbox: same data, persistent journal mode flipped to
// DELETE, no -wal file on disk.
func TestOpenDB_ConvertsExistingWALInSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.db")

	t.Setenv("SANDBOX_VM_ID", "")
	hostDB, err := OpenDB(t.Context(), path)
	require.NoError(t, err)
	_, err = hostDB.ExecContext(t.Context(), "CREATE TABLE t (id INTEGER); INSERT INTO t VALUES (42)")
	require.NoError(t, err)
	require.NoError(t, hostDB.Close())

	t.Setenv("SANDBOX_VM_ID", "vm-1")
	sandboxDB, err := OpenDB(t.Context(), path)
	require.NoError(t, err)
	defer sandboxDB.Close()

	var mode string
	require.NoError(t, sandboxDB.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "delete", mode)

	var id int
	require.NoError(t, sandboxDB.QueryRowContext(t.Context(), "SELECT id FROM t").Scan(&id))
	assert.Equal(t, 42, id)
	assert.NoFileExists(t, path+"-wal")
}

func TestIsTransientError(t *testing.T) {
	t.Parallel()

	assert.False(t, IsTransientError(nil))
	assert.False(t, IsTransientError(errors.New("boom")))
	assert.True(t, IsTransientError(context.Canceled))
	assert.True(t, IsTransientError(context.DeadlineExceeded))
	assert.True(t, IsTransientError(fmt.Errorf("wrapped: %w", context.Canceled)))
}

func TestIsTransientError_SQLiteBusy(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := path + "?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)"

	writer, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer writer.Close()

	tx, err := writer.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // test cleanup, error irrelevant
	_, err = tx.ExecContext(t.Context(), "CREATE TABLE t (id INTEGER)")
	require.NoError(t, err)

	// A second connection hits the write lock and gets SQLITE_BUSY.
	blocked, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer blocked.Close()

	_, err = blocked.ExecContext(t.Context(), "CREATE TABLE u (id INTEGER)")
	require.Error(t, err)
	assert.True(t, IsTransientError(err), "SQLITE_BUSY should be transient: %v", err)
	assert.False(t, IsCantOpenError(err))
}
