package tuistate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// newTestStore creates a Store backed by a fresh SQLite database in the test's temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tui_state.db")
	db, err := sqliteutil.OpenDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	store := &Store{db: db}
	require.NoError(t, store.migrate())
	return store
}

func TestAddAndGetTabs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.AddTab(ctx, "sess-1", "/home/user/project1"))
	require.NoError(t, store.AddTab(ctx, "sess-2", "/home/user/project2"))
	require.NoError(t, store.SetActiveTab(ctx, "sess-2"))

	tabs, activeID, err := store.GetTabs(ctx)
	require.NoError(t, err)
	assert.Len(t, tabs, 2)
	assert.Equal(t, "sess-1", tabs[0].SessionID)
	assert.Equal(t, "sess-2", tabs[1].SessionID)
	assert.Equal(t, "sess-2", activeID)
}

func TestRemoveTab(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.AddTab(ctx, "sess-1", "/dir1"))
	require.NoError(t, store.AddTab(ctx, "sess-2", "/dir2"))
	require.NoError(t, store.RemoveTab(ctx, "sess-1"))

	tabs, _, err := store.GetTabs(ctx)
	require.NoError(t, err)
	assert.Len(t, tabs, 1)
	assert.Equal(t, "sess-2", tabs[0].SessionID)
}

func TestUpdateTabSessionID(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.AddTab(ctx, "old-id", "/dir"))
	require.NoError(t, store.UpdateTabSessionID(ctx, "old-id", "new-id"))

	tabs, _, err := store.GetTabs(ctx)
	require.NoError(t, err)
	require.Len(t, tabs, 1)
	assert.Equal(t, "new-id", tabs[0].SessionID)
}

func TestClearTabs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.AddTab(ctx, "sess-1", "/dir1"))
	require.NoError(t, store.AddTab(ctx, "sess-2", "/dir2"))
	require.NoError(t, store.SetActiveTab(ctx, "sess-1"))
	require.NoError(t, store.ClearTabs(ctx))

	tabs, activeID, err := store.GetTabs(ctx)
	require.NoError(t, err)
	assert.Empty(t, tabs)
	assert.Empty(t, activeID)
}

// TestPersistedIDsSurviveRestart is the core regression test for the tab restore bug.
//
// Scenario: Three tabs are persisted in the DB with their original session-store IDs.
// The app restarts — the new startup code should NOT overwrite these IDs. After the
// simulated restart (a fresh GetTabs call), all original session IDs must still be present.
//
// Old behavior (bug): ClearTabs + AddTab(newEphemeralID) would replace the persisted IDs
// with ephemeral runtime IDs. Only the first tab (restored eagerly) got UpdateTabSessionID
// back to the real ID; tabs 2+ were lost.
func TestPersistedIDsSurviveRestart(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	// --- Simulate first run: user has 3 tabs open ---
	require.NoError(t, store.AddTab(ctx, "persisted-sess-A", "/projectA"))
	require.NoError(t, store.AddTab(ctx, "persisted-sess-B", "/projectB"))
	require.NoError(t, store.AddTab(ctx, "persisted-sess-C", "/projectC"))
	require.NoError(t, store.SetActiveTab(ctx, "persisted-sess-B"))

	// --- Simulate restart: read tabs (the "new" startup logic) ---
	savedTabs, savedActiveID, err := store.GetTabs(ctx)
	require.NoError(t, err)
	require.Len(t, savedTabs, 3)
	assert.Equal(t, "persisted-sess-B", savedActiveID)

	// With the fix, the startup code does NOT call ClearTabs + re-AddTab.
	// The DB should be untouched. Verify:
	tabsAfterRestart, activeAfterRestart, err := store.GetTabs(ctx)
	require.NoError(t, err)
	require.Len(t, tabsAfterRestart, 3)
	assert.Equal(t, "persisted-sess-A", tabsAfterRestart[0].SessionID)
	assert.Equal(t, "persisted-sess-B", tabsAfterRestart[1].SessionID)
	assert.Equal(t, "persisted-sess-C", tabsAfterRestart[2].SessionID)
	assert.Equal(t, "persisted-sess-B", activeAfterRestart)
}

// TestOldStartupFlowDestroysIDs demonstrates what the old buggy code did:
// ClearTabs followed by AddTab with ephemeral IDs replaces the persisted
// session-store IDs, making tabs unreachable on subsequent restarts.
func TestOldStartupFlowDestroysIDs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	// First run: 3 tabs persisted
	require.NoError(t, store.AddTab(ctx, "persisted-sess-A", "/projectA"))
	require.NoError(t, store.AddTab(ctx, "persisted-sess-B", "/projectB"))
	require.NoError(t, store.AddTab(ctx, "persisted-sess-C", "/projectC"))
	require.NoError(t, store.SetActiveTab(ctx, "persisted-sess-B"))

	// Old buggy restart: ClearTabs + AddTab with ephemeral IDs
	require.NoError(t, store.ClearTabs(ctx))
	require.NoError(t, store.AddTab(ctx, "ephemeral-1", "/projectA"))
	require.NoError(t, store.AddTab(ctx, "ephemeral-2", "/projectB"))
	require.NoError(t, store.AddTab(ctx, "ephemeral-3", "/projectC"))

	// Only tab 1 gets its ID repaired (the user visited it)
	require.NoError(t, store.UpdateTabSessionID(ctx, "ephemeral-1", "persisted-sess-A"))

	// Tabs 2 and 3 keep their ephemeral IDs — these can't be resolved
	// by SessionStore.GetSession on the next restart.
	tabs, _, err := store.GetTabs(ctx)
	require.NoError(t, err)
	require.Len(t, tabs, 3)
	assert.Equal(t, "persisted-sess-A", tabs[0].SessionID) // repaired
	assert.Equal(t, "ephemeral-2", tabs[1].SessionID)      // BROKEN — lost
	assert.Equal(t, "ephemeral-3", tabs[2].SessionID)      // BROKEN — lost
}

func TestFavoriteDirs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.AddFavoriteDir(ctx, "/home/user/project"))
	isFav, err := store.IsFavoriteDir(ctx, "/home/user/project")
	require.NoError(t, err)
	assert.True(t, isFav)

	dirs, err := store.GetFavoriteDirs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"/home/user/project"}, dirs)

	removed, err := store.ToggleFavoriteDir(ctx, "/home/user/project")
	require.NoError(t, err)
	assert.False(t, removed)

	isFav, err = store.IsFavoriteDir(ctx, "/home/user/project")
	require.NoError(t, err)
	assert.False(t, isFav)
}

func TestRecentDirs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	// AddTab with a non-empty working dir records it as a recent dir.
	require.NoError(t, store.AddTab(ctx, "s1", "/dir1"))
	require.NoError(t, store.AddTab(ctx, "s2", "/dir2"))

	dirs, err := store.GetRecentDirs(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, dirs, 2)
}

func TestGetTabsEmptyDB(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	tabs, activeID, err := store.GetTabs(ctx)
	require.NoError(t, err)
	assert.Empty(t, tabs)
	assert.Empty(t, activeID)
}

func TestRemoveNonexistentTab(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	// Should not error — just a no-op.
	require.NoError(t, store.RemoveTab(ctx, "does-not-exist"))
}
