package session

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openMemoryStore returns a SQLiteSessionStore backed by an in-memory database
// and a t.Cleanup that closes it. Tests use this to avoid the overhead of
// allocating a temp directory and writing WAL files to disk.
func openMemoryStore(t *testing.T) *SQLiteSessionStore {
	t.Helper()
	// SQLite ":memory:" databases are private to a single connection, so the
	// store's MaxOpenConns=1 setting (applied by sqliteutil for file DBs) is
	// implicitly satisfied here too. We open with database/sql directly so
	// the test does not depend on a working filesystem.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// Register the db cleanup before any potentially failing call so the
	// connection is released even if NewSQLiteSessionStoreFromDB returns an
	// error. Calling Close on an already-closed *sql.DB is a no-op, so the
	// store.Close() registered below is harmless when both run.
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	store, err := NewSQLiteSessionStoreFromDB(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestNewSQLiteSessionStoreFromDB_NilDB(t *testing.T) {
	_, err := NewSQLiteSessionStoreFromDB(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestNewSQLiteSessionStoreFromDB_RunsMigrations(t *testing.T) {
	store := openMemoryStore(t)

	// The applied migration list should be the full production list. We don't
	// pin the exact count here — just verify the store is usable end-to-end,
	// which proves the schema is in place.
	ctx := t.Context()
	session := New(WithID("from-db-1"), WithTitle("hello"))
	require.NoError(t, store.AddSession(ctx, session))

	got, err := store.GetSession(ctx, "from-db-1")
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Title)
}

func TestNewSQLiteSessionStoreFromDB_RoundTripWithMessages(t *testing.T) {
	store := openMemoryStore(t)
	ctx := t.Context()

	session := New(WithID("rt-1"), WithTitle("round trip"))
	require.NoError(t, store.AddSession(ctx, session))

	_, err := store.AddMessage(ctx, session.ID, UserMessage("hello"))
	require.NoError(t, err)
	_, err = store.AddMessage(ctx, session.ID, UserMessage("world"))
	require.NoError(t, err)

	got, err := store.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
	assert.Equal(t, "world", got.Messages[1].Message.Message.Content)
}
