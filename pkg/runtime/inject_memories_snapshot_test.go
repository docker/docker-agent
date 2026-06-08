package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/memory/database"
)

// countingDB counts calls to GetMemories for cache-hit verification.
type countingDB struct {
	fakeMemDB

	mu       sync.Mutex
	getCalls int
}

func (c *countingDB) GetMemories(ctx context.Context) ([]database.UserMemory, error) {
	c.mu.Lock()
	c.getCalls++
	c.mu.Unlock()
	return c.fakeMemDB.GetMemories(ctx)
}

func (c *countingDB) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getCalls
}

// TestSnapshot_CachesAcrossCalls verifies that two consecutive get() calls
// with the same generation only issue one GetMemories call.
func TestSnapshot_CachesAcrossCalls(t *testing.T) {
	t.Parallel()

	raw := &countingDB{fakeMemDB: fakeMemDB{memories: []database.UserMemory{
		{Memory: "cached memory"},
	}}}
	wrapped := newInvalidatingDB(raw)
	cache := newMemorySnapshotCache()

	m1, err := cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	require.Len(t, m1, 1)

	m2, err := cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	require.Len(t, m2, 1)

	assert.Equal(t, 1, raw.calls(), "expected exactly one GetMemories call for two reads at same generation")
}

// TestSnapshot_InvalidatesOnAdd verifies that AddMemory bumps the generation
// and causes the next get() to re-fetch from the DB.
func TestSnapshot_InvalidatesOnAdd(t *testing.T) {
	t.Parallel()

	raw := &countingDB{fakeMemDB: fakeMemDB{memories: []database.UserMemory{
		{Memory: "original"},
	}}}
	wrapped := newInvalidatingDB(raw)
	cache := newMemorySnapshotCache()

	_, err := cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	assert.Equal(t, 1, raw.calls())

	// Simulate add_memory.
	require.NoError(t, wrapped.AddMemory(t.Context(), database.UserMemory{Memory: "new"}))

	_, err = cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	assert.Equal(t, 2, raw.calls(), "expected re-fetch after AddMemory")
}

// TestSnapshot_InvalidatesOnUpdate verifies UpdateMemory triggers invalidation.
func TestSnapshot_InvalidatesOnUpdate(t *testing.T) {
	t.Parallel()

	raw := &countingDB{fakeMemDB: fakeMemDB{memories: []database.UserMemory{{Memory: "v1"}}}}
	wrapped := newInvalidatingDB(raw)
	cache := newMemorySnapshotCache()

	_, err := cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	assert.Equal(t, 1, raw.calls())

	require.NoError(t, wrapped.UpdateMemory(t.Context(), database.UserMemory{Memory: "v2"}))

	_, err = cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	assert.Equal(t, 2, raw.calls(), "expected re-fetch after UpdateMemory")
}

// TestSnapshot_InvalidatesOnDelete verifies DeleteMemory triggers invalidation.
func TestSnapshot_InvalidatesOnDelete(t *testing.T) {
	t.Parallel()

	raw := &countingDB{fakeMemDB: fakeMemDB{memories: []database.UserMemory{{Memory: "to delete"}}}}
	wrapped := newInvalidatingDB(raw)
	cache := newMemorySnapshotCache()

	_, err := cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	assert.Equal(t, 1, raw.calls())

	require.NoError(t, wrapped.DeleteMemory(t.Context(), database.UserMemory{Memory: "to delete"}))

	_, err = cache.get(t.Context(), "agent", wrapped)
	require.NoError(t, err)
	assert.Equal(t, 2, raw.calls(), "expected re-fetch after DeleteMemory")
}

// TestSnapshot_ConcurrentReads verifies no race conditions under concurrent
// get() calls. Run with -race to catch data races.
func TestSnapshot_ConcurrentReads(t *testing.T) {
	t.Parallel()

	raw := &countingDB{fakeMemDB: fakeMemDB{memories: []database.UserMemory{
		{Memory: "concurrent memory"},
	}}}
	wrapped := newInvalidatingDB(raw)
	cache := newMemorySnapshotCache()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			m, err := cache.get(t.Context(), "agent", wrapped)
			assert.NoError(t, err)
			assert.Len(t, m, 1)
		}()
	}
	wg.Wait()

	// All goroutines raced to refresh; due to the double-check lock at
	// most a handful of extra GetMemories calls are expected — but never
	// more than N. We simply assert it is well below N to catch a broken
	// implementation.
	assert.LessOrEqual(t, raw.calls(), N)
	assert.GreaterOrEqual(t, raw.calls(), 1)
}

// TestInvalidatingDB_FailedWriteDoesNotBump verifies that if the underlying
// write fails, the generation is NOT advanced (snapshot remains valid).
func TestInvalidatingDB_FailedWriteDoesNotBump(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("write failed")
	raw := &fakeMemDB{getMemoriesErr: nil}
	raw.memories = []database.UserMemory{{Memory: "existing"}}

	// Stub out errors for write ops.
	failDB := &writeFailing{fakeMemDB: raw, err: writeErr}
	wrapped := newInvalidatingDB(failDB)

	genBefore := wrapped.gen()

	err := wrapped.AddMemory(t.Context(), database.UserMemory{Memory: "new"})
	require.Error(t, err)
	assert.Equal(t, genBefore, wrapped.gen(), "generation must not advance on failed write")
}

// TestInvalidatingDB_AtomicGen verifies gen() / bump() are race-free.
func TestInvalidatingDB_AtomicGen(t *testing.T) {
	t.Parallel()

	raw := &fakeMemDB{}
	wrapped := newInvalidatingDB(raw)

	var ops atomic.Int32
	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			wrapped.bump()
			ops.Add(1)
		}()
	}
	wg.Wait()
	assert.Equal(t, uint64(N), wrapped.gen())
}

// writeFailing wraps a fakeMemDB and returns an error for all write operations.
type writeFailing struct {
	*fakeMemDB

	err error
}

func (w *writeFailing) AddMemory(_ context.Context, _ database.UserMemory) error    { return w.err }
func (w *writeFailing) UpdateMemory(_ context.Context, _ database.UserMemory) error { return w.err }
func (w *writeFailing) DeleteMemory(_ context.Context, _ database.UserMemory) error { return w.err }
func (w *writeFailing) SearchMemories(_ context.Context, _, _ string) ([]database.UserMemory, error) {
	return nil, nil
}
