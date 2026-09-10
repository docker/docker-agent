package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/sqliteutil"
)

func newConcreteSQLiteStoreForCompactionTest(t *testing.T) *SQLiteSessionStore {
	t.Helper()
	db, err := sqliteutil.OpenDB(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func newInMemoryStoreForCompactionTest(t *testing.T) Store {
	t.Helper()
	return NewInMemorySessionStore()
}

func newSQLiteStoreForCompactionTest(t *testing.T) Store {
	t.Helper()
	return newConcreteSQLiteStoreForCompactionTest(t)
}

func TestPersistCompactionStoresResultingCost(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) Store
	}{
		{name: "memory", store: newInMemoryStoreForCompactionTest},
		{name: "sqlite", store: newSQLiteStoreForCompactionTest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.store(t)
			sess := New(WithID("cost"), WithMessages([]Item{
				NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "prior", Cost: 1.25}}),
			}))
			// Deliberately stale legacy scalar: canonical cost comes from items.
			sess.SetTokensAndCost(100, 20, 99)
			require.NoError(t, store.AddSession(t.Context(), sess))

			require.NoError(t, store.PersistCompaction(t.Context(), sess, 7, 0, Item{Summary: "summary", Cost: 0.75}))
			reloaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			input, output, cost := reloaded.TokensAndCost()
			assert.Equal(t, int64(7), input)
			assert.Zero(t, output)
			assert.InDelta(t, 2.0, cost, 1e-9)
			require.Len(t, reloaded.Messages, 2)
			assert.InDelta(t, 0.75, reloaded.Messages[1].Cost, 1e-9)
		})
	}
}

func TestPersistCompactionMissingRowUpsertsAcrossStores(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) Store
	}{
		{name: "memory", store: newInMemoryStoreForCompactionTest},
		{name: "sqlite", store: newSQLiteStoreForCompactionTest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.store(t)
			sess := New(WithID("missing"))
			sess.SetTokensAndCost(50, 10, 0.5)
			require.NoError(t, store.PersistCompaction(t.Context(), sess, 5, 0, Item{Summary: "summary", Cost: 0.25}))

			reloaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			input, output, cost := reloaded.TokensAndCost()
			assert.Equal(t, int64(5), input)
			assert.Zero(t, output)
			assert.InDelta(t, 0.25, cost, 1e-9)
			require.Len(t, reloaded.Messages, 1)
			assert.Equal(t, "summary", reloaded.Messages[0].Summary)
		})
	}
}

func TestSQLitePersistCompactionSnapshotsConcurrentMetadata(t *testing.T) {
	store := newConcreteSQLiteStoreForCompactionTest(t)
	sess := New(WithID("race"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			sess.SetTitle("title")
			sess.SetToolsApproved(i%2 == 0)
			sess.SetSafetyPolicy(SafetyPolicy("standard"))
			sess.SetAttribute("iteration", "value")
			sess.SetPermissions(&PermissionsConfig{})
		}
	})
	for range 50 {
		err := store.PersistCompaction(t.Context(), sess, 7, 0, Item{Summary: "summary"})
		require.NoError(t, err)
	}
	wg.Wait()
}

func TestSQLitePersistCompactionRollsBackMetadataWhenSummaryInsertFails(t *testing.T) {
	store := newConcreteSQLiteStoreForCompactionTest(t)
	sess := New(WithID("rollback"))
	sess.SetTokensAndCost(100, 20, 1.25)
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, func() error {
		_, err := store.db.ExecContext(t.Context(), `
			CREATE TRIGGER fail_compaction_summary
			BEFORE INSERT ON session_items
			WHEN NEW.item_type = 'summary'
			BEGIN
				SELECT RAISE(ABORT, 'injected summary failure');
			END`)
		return err
	}())

	err := store.PersistCompaction(t.Context(), sess, 7, 0, Item{Summary: "must roll back", Cost: 0.75})
	require.ErrorContains(t, err, "injected summary failure")

	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	input, output, cost := reloaded.TokensAndCost()
	assert.Equal(t, int64(100), input)
	assert.Equal(t, int64(20), output)
	assert.InDelta(t, 1.25, cost, 1e-9)
	assert.Empty(t, reloaded.Messages)
	assert.Empty(t, sess.Messages, "live state must not mutate before transaction commit")
}
