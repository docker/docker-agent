package session

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openMigrationsDB returns a fresh in-memory database with the migrations
// table already created. Tests that exercise MigrationManager use it instead
// of the production setupAndMigrate helper so the manager is the only thing
// under test.
func openMigrationsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	mgr := NewMigrationManagerWithMigrations(db, nil)
	require.NoError(t, mgr.createMigrationsTable(t.Context()))
	return db
}

func TestMigrationManager_RunPendingMigrations_AppliesAllAndIsIdempotent(t *testing.T) {
	db := openMigrationsDB(t)
	migrations := []Migration{
		{
			ID:    1,
			Name:  "001_create_widgets",
			UpSQL: `CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT)`,
		},
		{
			ID:    2,
			Name:  "002_add_color_column",
			UpSQL: `ALTER TABLE widgets ADD COLUMN color TEXT DEFAULT ''`,
		},
	}
	mgr := NewMigrationManagerWithMigrations(db, migrations)
	ctx := t.Context()

	require.NoError(t, mgr.RunPendingMigrations(ctx))

	applied, err := mgr.GetAppliedMigrations(ctx)
	require.NoError(t, err)
	require.Len(t, applied, 2)
	assert.Equal(t, "001_create_widgets", applied[0].Name)
	assert.Equal(t, "002_add_color_column", applied[1].Name)

	// Schema is in place: insert and select work.
	_, err = db.ExecContext(ctx, `INSERT INTO widgets (id, name, color) VALUES (1, 'sprocket', 'red')`)
	require.NoError(t, err)

	// Running again is a no-op.
	require.NoError(t, mgr.RunPendingMigrations(ctx))
	applied2, err := mgr.GetAppliedMigrations(ctx)
	require.NoError(t, err)
	assert.Len(t, applied2, 2)
}

func TestMigrationManager_RunPendingMigrations_OnlyAppliesNewMigrations(t *testing.T) {
	db := openMigrationsDB(t)
	first := []Migration{{ID: 1, Name: "001_a", UpSQL: `CREATE TABLE a (id INTEGER)`}}
	require.NoError(t, NewMigrationManagerWithMigrations(db, first).RunPendingMigrations(t.Context()))

	// Bring in the second migration; only it should be applied.
	both := []Migration{
		first[0],
		{ID: 2, Name: "002_b", UpSQL: `CREATE TABLE b (id INTEGER)`},
	}
	mgr := NewMigrationManagerWithMigrations(db, both)
	require.NoError(t, mgr.RunPendingMigrations(t.Context()))

	applied, err := mgr.GetAppliedMigrations(t.Context())
	require.NoError(t, err)
	assert.Len(t, applied, 2)

	// Both tables must exist; if 001 had been re-run we would have got an error.
	_, err = db.ExecContext(t.Context(), `INSERT INTO a (id) VALUES (1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO b (id) VALUES (1)`)
	require.NoError(t, err)
}

func TestMigrationManager_BadUpSQL_RollsBackAndIsRetryable(t *testing.T) {
	db := openMigrationsDB(t)
	bad := []Migration{{
		ID:    1,
		Name:  "001_bad",
		UpSQL: `THIS IS NOT VALID SQL`,
	}}
	mgr := NewMigrationManagerWithMigrations(db, bad)

	err := mgr.RunPendingMigrations(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "001_bad")

	// The transaction must have been rolled back, so the migration is NOT
	// recorded and the manager will retry it next time.
	applied, err := mgr.GetAppliedMigrations(t.Context())
	require.NoError(t, err)
	assert.Empty(t, applied, "failed migration must not be recorded")
}

func TestMigrationManager_UpFuncFailure_DoesNotRetryButReturnsError(t *testing.T) {
	db := openMigrationsDB(t)
	wantErr := errors.New("boom")
	mgr := NewMigrationManagerWithMigrations(db, []Migration{{
		ID:   1,
		Name: "001_func",
		UpFunc: func(_ context.Context, _ *sql.DB) error {
			return wantErr
		},
	}})

	err := mgr.RunPendingMigrations(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, wantErr)

	// The current implementation commits the SQL phase before running UpFunc,
	// so the migration row IS recorded even if UpFunc fails. This test pins
	// that behaviour so any change to it (e.g. wrapping UpFunc in the same
	// transaction) is intentional.
	applied, err := mgr.GetAppliedMigrations(t.Context())
	require.NoError(t, err)
	assert.Len(t, applied, 1)
}

func TestMigrationManager_CheckForUnknownMigrations_RejectsNewerDB(t *testing.T) {
	db := openMigrationsDB(t)
	// Apply two migrations as a "newer" docker-agent would.
	full := []Migration{
		{ID: 1, Name: "001_a", UpSQL: `CREATE TABLE a (id INTEGER)`},
		{ID: 2, Name: "002_b", UpSQL: `CREATE TABLE b (id INTEGER)`},
	}
	require.NoError(t, NewMigrationManagerWithMigrations(db, full).RunPendingMigrations(t.Context()))

	// Now an "older" binary only knows about migration 1.
	older := NewMigrationManagerWithMigrations(db, full[:1])
	err := older.checkForUnknownMigrations(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNewerDatabase)
}

func TestMigrationManager_CheckForUnknownMigrations_AllowsEqualOrOlderDB(t *testing.T) {
	db := openMigrationsDB(t)
	migrations := []Migration{{ID: 1, Name: "001_a", UpSQL: `CREATE TABLE a (id INTEGER)`}}
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrations).RunPendingMigrations(t.Context()))

	// Same set: fine.
	require.NoError(t, NewMigrationManagerWithMigrations(db, migrations).checkForUnknownMigrations(t.Context()))

	// Newer binary that knows about more migrations than the DB has applied: fine.
	newer := slices.Clone(migrations)
	newer = append(newer, Migration{ID: 2, Name: "002_b", UpSQL: `CREATE TABLE b (id INTEGER)`})
	require.NoError(t, NewMigrationManagerWithMigrations(db, newer).checkForUnknownMigrations(t.Context()))
}

func TestMigrationManager_EmptyMigrations_NoOp(t *testing.T) {
	db := openMigrationsDB(t)
	mgr := NewMigrationManagerWithMigrations(db, nil)

	require.NoError(t, mgr.RunPendingMigrations(t.Context()))
	require.NoError(t, mgr.checkForUnknownMigrations(t.Context()))
}
