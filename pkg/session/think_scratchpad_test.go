package session

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scratchpadStores(t *testing.T, check func(*testing.T, Store)) {
	t.Helper()
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			store := NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			check(t, store)
		})
	}
}

func TestThinkScratchpadStore(t *testing.T) {
	t.Parallel()
	scratchpadStores(t, func(t *testing.T, store Store) {
		t.Helper()
		ctx := t.Context()
		appendStore := store.(ThinkScratchpadStore)
		root := New()
		key := ScratchpadKey{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:0"}
		_, err := appendStore.AppendThought(ctx, root.ID, key, "before persistence")
		require.ErrorIs(t, err, ErrScratchpadOwner)
		require.NoError(t, store.UpdateSession(ctx, root))
		stale, err := store.GetSession(ctx, root.ID)
		require.NoError(t, err)
		stale = stale.Clone()
		stale.Origin = root.Origin
		out, err := appendStore.AppendThought(ctx, root.ID, key, "first\nline")
		require.NoError(t, err)
		require.Equal(t, []string{"first\nline"}, out)
		out[0] = "caller mutation"
		require.NoError(t, store.UpdateSession(ctx, stale))
		require.NoError(t, store.PersistCompaction(ctx, stale, 0, 0, Item{Summary: "compacted"}))
		out, err = appendStore.AppendThought(ctx, root.ID, key, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"first\nline", ""}, out)
		for _, other := range []ScratchpadKey{
			{SessionID: root.ID, AgentName: "worker", ToolsetKey: "cfg:0"},
			{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:1"},
			{SessionID: "unpersisted-child", AgentName: "root", ToolsetKey: "cfg:0"},
		} {
			out, err := appendStore.AppendThought(ctx, root.ID, other, "isolated")
			require.NoError(t, err)
			assert.Equal(t, []string{"isolated"}, out)
		}
		otherRoot := New()
		require.NoError(t, store.UpdateSession(ctx, otherRoot))
		_, err = appendStore.AppendThought(ctx, otherRoot.ID, key, "wrong owner")
		require.ErrorIs(t, err, ErrScratchpadOwner)
		wrongKey := key
		wrongKey.AgentName = "different-agent"
		_, err = appendStore.AppendThought(ctx, otherRoot.ID, wrongKey, "wrong root with fresh key")
		require.ErrorIs(t, err, ErrScratchpadOwner)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = appendStore.AppendThought(cancelled, root.ID, key, "cancelled")
		require.ErrorIs(t, err, context.Canceled)
		out, err = appendStore.AppendThought(ctx, root.ID, key, "last")
		require.NoError(t, err)
		assert.Equal(t, []string{"first\nline", "", "last"}, out)
	})
}

func TestThinkScratchpadConcurrent(t *testing.T) {
	t.Parallel()
	scratchpadStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root := New()
		require.NoError(t, store.UpdateSession(t.Context(), root))
		appender := store.(ThinkScratchpadStore)
		key := ScratchpadKey{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:0"}
		const n = 64
		outputs := make([][]string, n)
		errs := make([]error, n)
		thoughts := make([]string, n)
		var wg sync.WaitGroup
		for i := range n {
			thoughts[i] = fmt.Sprintf("thought-%d", i)
			wg.Go(func() { outputs[i], errs[i] = appender.AppendThought(t.Context(), root.ID, key, thoughts[i]) })
		}
		wg.Wait()
		lengths := make(map[int]bool)
		for i := range n {
			require.NoError(t, errs[i])
			require.Equal(t, thoughts[i], outputs[i][len(outputs[i])-1])
			lengths[len(outputs[i])] = true
		}
		require.Len(t, lengths, n)
		out, err := appender.AppendThought(t.Context(), root.ID, key, "final")
		require.NoError(t, err)
		require.Len(t, out, n+1)
		assert.ElementsMatch(t, thoughts, out[:n])
	})
}

func TestThinkScratchpadDeletion(t *testing.T) {
	t.Parallel()
	scratchpadStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root := New()
		child := New()
		child.ParentID = root.ID
		grandchild := New()
		grandchild.ParentID = child.ID
		for _, sess := range []*Session{root, child, grandchild} {
			require.NoError(t, store.UpdateSession(t.Context(), sess))
		}
		appender := store.(ThinkScratchpadStore)
		keys := []ScratchpadKey{
			{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:0"},
			{SessionID: child.ID, AgentName: "worker", ToolsetKey: "cfg:0"},
			{SessionID: grandchild.ID, AgentName: "worker", ToolsetKey: "cfg:0"},
			{SessionID: "aborted", AgentName: "worker", ToolsetKey: "cfg:0"},
		}
		for _, key := range keys {
			_, err := appender.AppendThought(t.Context(), root.ID, key, "initial")
			require.NoError(t, err)
		}
		require.NoError(t, store.DeleteSession(t.Context(), child.ID))
		for _, key := range keys[1:3] {
			out, err := appender.AppendThought(t.Context(), root.ID, key, "after child deletion")
			require.NoError(t, err)
			assert.Equal(t, []string{"after child deletion"}, out)
		}
		out, err := appender.AppendThought(t.Context(), root.ID, keys[3], "still root owned")
		require.NoError(t, err)
		assert.Equal(t, []string{"initial", "still root owned"}, out)
		require.NoError(t, store.DeleteSession(t.Context(), root.ID))
		_, err = appender.AppendThought(t.Context(), root.ID, keys[3], "no resurrection")
		require.ErrorIs(t, err, ErrScratchpadOwner)
		require.NoError(t, store.UpdateSession(t.Context(), root))
		for _, key := range keys {
			out, err := appender.AppendThought(t.Context(), root.ID, key, "after root deletion")
			require.NoError(t, err)
			assert.Equal(t, []string{"after root deletion"}, out)
		}
	})
}

func TestThinkScratchpadSQLiteReopenAndMultipleHandles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	root := New()
	require.NoError(t, store.UpdateSession(t.Context(), root))
	key := ScratchpadKey{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:0"}
	_, err = store.(ThinkScratchpadStore).AppendThought(t.Context(), root.ID, key, "before restart")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	second, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Go(func() {
			writer := store
			if i%2 == 0 {
				writer = second
			}
			_, errs[i] = writer.(ThinkScratchpadStore).AppendThought(t.Context(), root.ID, key, strconv.Itoa(i))
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	out, err := store.(ThinkScratchpadStore).AppendThought(t.Context(), root.ID, key, "end")
	require.NoError(t, err)
	require.Len(t, out, 22)
	assert.Equal(t, "before restart", out[0])
	assert.Equal(t, "end", out[21])
}

func TestThinkScratchpadMigrationUpgrade(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	sqlite := store.(*SQLiteSessionStore)
	root := New()
	require.NoError(t, store.UpdateSession(t.Context(), root))
	_, err = sqlite.db.ExecContext(t.Context(), `DROP TABLE think_scratchpad; DELETE FROM migrations WHERE id = 31`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	out, err := store.(ThinkScratchpadStore).AppendThought(t.Context(), root.ID, ScratchpadKey{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:0"}, "upgraded")
	require.NoError(t, err)
	assert.Equal(t, []string{"upgraded"}, out)
	mgr := NewMigrationManager(store.(*SQLiteSessionStore).db)
	require.NoError(t, mgr.InitializeMigrations(t.Context()))
}

func TestThinkScratchpadDeleteRollback(t *testing.T) {
	t.Parallel()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, child := New(), New()
	child.ParentID = root.ID
	for _, sess := range []*Session{root, child} {
		require.NoError(t, store.UpdateSession(t.Context(), sess))
	}
	key := ScratchpadKey{SessionID: child.ID, AgentName: "worker", ToolsetKey: "cfg:0"}
	_, err = store.(ThinkScratchpadStore).AppendThought(t.Context(), root.ID, key, "retained")
	require.NoError(t, err)
	_, err = store.(*SQLiteSessionStore).db.ExecContext(t.Context(), `CREATE TRIGGER prevent_session_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'delete blocked'); END;`)
	require.NoError(t, err)
	require.Error(t, store.DeleteSession(t.Context(), child.ID))
	out, err := store.(ThinkScratchpadStore).AppendThought(t.Context(), root.ID, key, "after failed delete")
	require.NoError(t, err)
	assert.Equal(t, []string{"retained", "after failed delete"}, out)
}

func TestThinkScratchpadSQLiteDeletionWithoutForeignKeys(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	var foreignKeys int
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys))
	require.Zero(t, foreignKeys)

	roots := []*Session{New(), New()}
	keys := make([][]ScratchpadKey, len(roots))
	for i, root := range roots {
		require.NoError(t, store.UpdateSession(t.Context(), root))
		keys[i] = []ScratchpadKey{
			{SessionID: root.ID, AgentName: "root", ToolsetKey: "cfg:0"},
			{SessionID: root.ID + "-unpersisted", AgentName: "worker", ToolsetKey: "cfg:0"},
		}
		for _, key := range keys[i] {
			_, err := store.AppendThought(t.Context(), root.ID, key, "initial")
			require.NoError(t, err)
		}
	}
	require.NoError(t, store.DeleteSession(t.Context(), roots[0].ID))
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM think_scratchpad WHERE root_id = ?", roots[0].ID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM think_scratchpad").Scan(&count))
	require.Equal(t, 2, count)
	for _, key := range keys[1] {
		out, err := store.AppendThought(t.Context(), roots[1].ID, key, "unrelated survives")
		require.NoError(t, err)
		require.Equal(t, []string{"initial", "unrelated survives"}, out)
	}
	require.NoError(t, store.DeleteSession(t.Context(), roots[1].ID))
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM think_scratchpad").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, store.UpdateSession(t.Context(), roots[0]))
	for _, key := range keys[0] {
		out, err := store.AppendThought(t.Context(), roots[0].ID, key, "recreated")
		require.NoError(t, err)
		require.Equal(t, []string{"recreated"}, out)
	}
}

func TestThinkScratchpadOwnershipAcrossKeys(t *testing.T) {
	t.Parallel()
	scratchpadStores(t, func(t *testing.T, store Store) {
		t.Helper()
		roots := []*Session{New(), New()}
		for _, root := range roots {
			require.NoError(t, store.UpdateSession(t.Context(), root))
		}
		appender := store.(ThinkScratchpadStore)
		for _, agent := range []string{"root", "worker"} {
			for _, toolset := range []string{"cfg:0", "cfg:1"} {
				key := ScratchpadKey{SessionID: "unpersisted", AgentName: agent, ToolsetKey: toolset}
				out, err := appender.AppendThought(t.Context(), roots[0].ID, key, "first")
				require.NoError(t, err)
				require.Equal(t, []string{"first"}, out)
				_, err = appender.AppendThought(t.Context(), roots[1].ID, key, "wrong owner")
				require.ErrorIs(t, err, ErrScratchpadOwner)
			}
		}
	})
}

func TestThinkScratchpadOwnershipDeletion(t *testing.T) {
	t.Parallel()
	scratchpadStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, otherRoot, child := New(), New(), New()
		for _, sess := range []*Session{root, otherRoot} {
			require.NoError(t, store.UpdateSession(t.Context(), sess))
		}
		require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
		appender := store.(ThinkScratchpadStore)
		keys := []ScratchpadKey{
			{SessionID: child.ID, AgentName: "worker", ToolsetKey: "cfg:0"},
			{SessionID: "unpersisted", AgentName: "worker", ToolsetKey: "cfg:0"},
		}
		unrelated := ScratchpadKey{SessionID: otherRoot.ID, AgentName: "root", ToolsetKey: "cfg:0"}
		for _, key := range append([]ScratchpadKey{unrelated}, keys...) {
			owner := root.ID
			if key == unrelated {
				owner = otherRoot.ID
			}
			_, err := appender.AppendThought(t.Context(), owner, key, "initial")
			require.NoError(t, err)
		}
		if memory, ok := store.(*InMemorySessionStore); ok {
			require.Equal(t, map[string]string{child.ID: root.ID, "unpersisted": root.ID, otherRoot.ID: otherRoot.ID}, memory.scratchpadOwners)
		}
		require.NoError(t, store.DeleteSession(t.Context(), child.ID))
		if memory, ok := store.(*InMemorySessionStore); ok {
			require.Equal(t, map[string]string{"unpersisted": root.ID, otherRoot.ID: otherRoot.ID}, memory.scratchpadOwners)
		}
		out, err := appender.AppendThought(t.Context(), otherRoot.ID, keys[0], "new owner after child deletion")
		require.NoError(t, err)
		require.Equal(t, []string{"new owner after child deletion"}, out)
		_, err = appender.AppendThought(t.Context(), otherRoot.ID, keys[1], "still root owned")
		require.ErrorIs(t, err, ErrScratchpadOwner)
		require.NoError(t, store.DeleteSession(t.Context(), root.ID))
		if memory, ok := store.(*InMemorySessionStore); ok {
			require.Equal(t, map[string]string{otherRoot.ID: otherRoot.ID}, memory.scratchpadOwners)
		}
		out, err = appender.AppendThought(t.Context(), otherRoot.ID, keys[1], "new owner after root deletion")
		require.NoError(t, err)
		require.Equal(t, []string{"new owner after root deletion"}, out)
		out, err = appender.AppendThought(t.Context(), otherRoot.ID, unrelated, "unrelated survives")
		require.NoError(t, err)
		require.Equal(t, []string{"initial", "unrelated survives"}, out)
	})
}

func TestThinkScratchpadRejectedAppendDoesNotClaimOwnership(t *testing.T) {
	t.Parallel()
	scratchpadStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, child := New(), New()
		require.NoError(t, store.UpdateSession(t.Context(), root))
		require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		appender := store.(ThinkScratchpadStore)
		for _, tc := range []struct {
			name      string
			cancelled bool
			owner     string
			err       error
		}{
			{"missing", false, "missing-root", ErrScratchpadOwner},
			{"non-root", false, child.ID, ErrScratchpadOwner},
			{"cancelled", true, root.ID, context.Canceled},
		} {
			t.Run(tc.name, func(t *testing.T) {
				key := ScratchpadKey{SessionID: tc.name, AgentName: "worker", ToolsetKey: "cfg:0"}
				ctx := t.Context()
				if tc.cancelled {
					ctx = cancelled
				}
				_, err := appender.AppendThought(ctx, tc.owner, key, "rejected")
				require.ErrorIs(t, err, tc.err)
				if memory, ok := store.(*InMemorySessionStore); ok {
					require.NotContains(t, memory.scratchpadOwners, key.SessionID)
				}
				otherRoot := New()
				require.NoError(t, store.UpdateSession(t.Context(), otherRoot))
				out, err := appender.AppendThought(t.Context(), otherRoot.ID, key, "accepted")
				require.NoError(t, err)
				require.Equal(t, []string{"accepted"}, out)
			})
		}
	})
}
