package runtime

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/memory/database"
	memory "github.com/docker/docker-agent/pkg/tools/builtin/memory"
)

// memorySnapshotCache holds frozen copies of each agent's memory list,
// keyed by agent name. The cache is invalidated by bumping a per-agent
// generation counter via invalidatingDB; subsequent reads detect the bump
// and refresh from the underlying DB.
//
// Concurrency model: the cache is read-mostly (one read per turn start),
// so an RWMutex with a double-checked write path is used. A narrow race
// between a concurrent AddMemory and a get() is safe: at worst an extra
// GetMemories call is issued — the double-check inside the write lock
// prevents duplicate refreshes.
type memorySnapshotCache struct {
	mu      sync.RWMutex
	entries map[string]*memorySnapshotEntry
}

type memorySnapshotEntry struct {
	// gen is the generation at which snap was taken. If the DB's
	// generation counter has advanced, snap is stale and must be
	// refreshed.
	gen  uint64
	snap []database.UserMemory
}

func newMemorySnapshotCache() *memorySnapshotCache {
	return &memorySnapshotCache{entries: make(map[string]*memorySnapshotEntry)}
}

// get returns the current snapshot for agentName, refreshing from db when
// the cached entry is missing or stale (db's generation has advanced).
func (c *memorySnapshotCache) get(ctx context.Context, agentName string, db *invalidatingDB) ([]database.UserMemory, error) {
	currentGen := db.gen()

	c.mu.RLock()
	e := c.entries[agentName]
	c.mu.RUnlock()
	if e != nil && e.gen == currentGen {
		return e.snap, nil
	}

	// Refresh under write lock with double-check to avoid duplicate
	// GetMemories calls when multiple goroutines race to refresh.
	c.mu.Lock()
	defer c.mu.Unlock()
	e = c.entries[agentName]
	if e != nil && e.gen == currentGen {
		return e.snap, nil
	}

	fresh, err := db.GetMemories(ctx)
	if err != nil {
		return nil, err
	}
	c.entries[agentName] = &memorySnapshotEntry{gen: currentGen, snap: fresh}
	return fresh, nil
}

// invalidatingDB wraps a memory.DB and bumps an atomic generation counter on
// any write (AddMemory, UpdateMemory, DeleteMemory). The runtime installs
// this wrapper once per agent (via lookupMemoryDB → mt.SetDB(wrapped)), so
// writes that go through the agent's own memory tools also advance the
// counter and trigger snapshot invalidation on the next turn.
//
// Note: this only covers writes that go through the wrapped instance's
// methods. External writes (e.g. direct SQLite file manipulation) will not
// advance the counter; the snapshot may go stale in that scenario. Direct
// SQLite access is not a supported runtime operation and is documented here
// as the only known counter-invalidation gap.
type invalidatingDB struct {
	memory.DB

	genVal atomic.Uint64
}

func newInvalidatingDB(db memory.DB) *invalidatingDB {
	return &invalidatingDB{DB: db}
}

func (d *invalidatingDB) gen() uint64 { return d.genVal.Load() }
func (d *invalidatingDB) bump()       { d.genVal.Add(1) }

func (d *invalidatingDB) AddMemory(ctx context.Context, m database.UserMemory) error {
	if err := d.DB.AddMemory(ctx, m); err != nil {
		return err
	}
	d.bump()
	return nil
}

func (d *invalidatingDB) UpdateMemory(ctx context.Context, m database.UserMemory) error {
	if err := d.DB.UpdateMemory(ctx, m); err != nil {
		return err
	}
	d.bump()
	return nil
}

func (d *invalidatingDB) DeleteMemory(ctx context.Context, m database.UserMemory) error {
	if err := d.DB.DeleteMemory(ctx, m); err != nil {
		return err
	}
	d.bump()
	return nil
}
