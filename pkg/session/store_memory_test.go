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

	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestNewSQLiteSessionStoreFromDB_NilDB(t *testing.T) {
	t.Parallel()
	_, err := NewSQLiteSessionStoreFromDB(t.Context(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestNewSQLiteSessionStoreFromDB_RunsMigrations(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestSessionOriginCannotChange verifies both stores reject an upsert that
// attempts to reclassify a persisted session.
func TestSessionOriginCannotChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for name, newStore := range map[string]func() Store{
		"in memory": NewInMemorySessionStore,
		"sqlite":    func() Store { return openMemoryStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore()
			existing := New(WithID("origin-immutable"), WithOrigin("run"), WithTitle("original"))
			require.NoError(t, store.AddSession(ctx, existing))

			replacement := New(WithID(existing.ID), WithOrigin("a2a"), WithTitle("replacement"))
			require.ErrorIs(t, store.UpdateSession(ctx, replacement), ErrOriginMismatch)

			got, err := store.GetSession(ctx, existing.ID)
			require.NoError(t, err)
			require.Equal(t, "run", got.Origin)
			require.Equal(t, "original", got.Title)
		})
	}
}

// TestAddSessionRejectsConflictingOrigin verifies that adding a duplicate ID cannot
// reclassify an existing session, including in the SQLite store used by A2A.
func TestAddSessionRejectsConflictingOrigin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for name, newStore := range map[string]func() Store{
		"in memory": NewInMemorySessionStore,
		"sqlite":    func() Store { return openMemoryStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore()
			existing := New(WithID("origin-add-conflict"), WithOrigin("run"), WithTitle("original"))
			require.NoError(t, store.AddSession(ctx, existing))

			err := store.AddSession(ctx, New(WithID(existing.ID), WithOrigin("a2a"), WithTitle("replacement")))
			require.Error(t, err)
			if name == "in memory" {
				require.ErrorIs(t, err, ErrAlreadyExists)
			}

			got, err := store.GetSession(ctx, existing.ID)
			require.NoError(t, err)
			assert.Equal(t, "run", got.Origin)
			assert.Equal(t, "original", got.Title)
		})
	}
}

func TestSessionOriginRoundTripAndLookup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for name, newStore := range map[string]func() Store{
		"in memory": NewInMemorySessionStore,
		"sqlite":    func() Store { return openMemoryStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore()
			runSession := New(WithID("run-session"))
			a2aSession := New(WithID("a2a-session"), WithOrigin("a2a"))
			require.NoError(t, store.AddSession(ctx, runSession))
			require.NoError(t, store.AddSession(ctx, a2aSession))

			got, err := store.GetSessionByOrigin(ctx, a2aSession.ID, "a2a")
			require.NoError(t, err)
			assert.Equal(t, "a2a", got.Origin)

			_, err = store.GetSessionByOrigin(ctx, runSession.ID, "a2a")
			require.ErrorIs(t, err, ErrNotFound)
			_, err = store.GetSessionByOrigin(ctx, "", "a2a")
			require.ErrorIs(t, err, ErrEmptyID)

			a2aSession.SetTitle("updated")
			require.NoError(t, store.UpdateSession(ctx, a2aSession))
			got, err = store.GetSession(ctx, a2aSession.ID)
			require.NoError(t, err)
			assert.Equal(t, "a2a", got.Origin)
		})
	}
}

// TestMigration23_LegacySummaryRowsReadAsZeroCost simulates a database
// created before migration 023 (no cost column on session_items, summary
// rows written without one): opening the store applies the migration and
// the legacy summary must hydrate with cost 0 — historical summary costs
// cannot be reconstructed.
func TestMigration23_LegacySummaryRowsReadAsZeroCost(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	// Bootstrap schema plus migrations 1..22 only — the pre-cost layout.
	_, err = db.ExecContext(ctx, `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	all := getAllMigrations()
	require.NoError(t, NewMigrationManagerWithMigrations(db, all[:len(all)-1]).InitializeMigrations(ctx))

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at) VALUES ('legacy', '2024-01-01T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO session_items (session_id, position, item_type, summary_text, first_kept_entry)
		 VALUES ('legacy', 0, 'summary', 'old summary', 2)`)
	require.NoError(t, err)

	store, err := NewSQLiteSessionStoreFromDB(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.GetSession(ctx, "legacy")
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "old summary", got.Messages[0].Summary)
	assert.Equal(t, 2, got.Messages[0].FirstKeptEntry)
	assert.Zero(t, got.Messages[0].Cost, "legacy summary rows must read as cost 0")
	assert.Equal(t, "run", got.Origin, "pre-origin sessions must be classified as normal runs")
}
