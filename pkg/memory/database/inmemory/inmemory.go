// Package inmemory provides a process-local database.Database for platforms
// where sqlite is unavailable (js/wasm). It follows the sqlite implementation:
// insertion order, AND keyword search, and the same sentinel errors. Contents
// are lost when the process exits.
//
// Case folding is not identical: sqlite's LOWER() only folds ASCII, whereas
// this package uses Go's Unicode-aware folding, so non-ASCII queries may
// match here and not in sqlite.
package inmemory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/memory/database"
)

var ErrDuplicateID = errors.New("memory ID already exists")

type Database struct {
	mu       sync.RWMutex
	memories []database.UserMemory
}

var _ database.Database = (*Database)(nil)

func New() *Database {
	return &Database{}
}

func (d *Database) AddMemory(ctx context.Context, memory database.UserMemory) error {
	if memory.ID == "" {
		return database.ErrEmptyID
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.indexOf(memory.ID) >= 0 {
		return fmt.Errorf("%w: %s", ErrDuplicateID, memory.ID)
	}
	d.memories = append(d.memories, memory)
	return nil
}

func (d *Database) GetMemories(ctx context.Context) ([]database.UserMemory, error) {
	return d.SearchMemories(ctx, "", "")
}

func (d *Database) DeleteMemory(ctx context.Context, memory database.UserMemory) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.memories = slices.DeleteFunc(d.memories, func(m database.UserMemory) bool {
		return m.ID == memory.ID
	})
	return nil
}

// SearchMemories returns memories whose content contains every whitespace
// separated word of query and whose category equals category, both compared
// case-insensitively (Unicode folding, see package doc). Empty query or
// category disables that filter. Like the sqlite implementation, it returns a
// nil slice when nothing matches.
func (d *Database) SearchMemories(ctx context.Context, query, category string) ([]database.UserMemory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	words := strings.Fields(strings.ToLower(query))

	d.mu.RLock()
	defer d.mu.RUnlock()

	var results []database.UserMemory
	for _, m := range d.memories {
		if matches(m, words, category) {
			results = append(results, m)
		}
	}
	return results, nil
}

func matches(m database.UserMemory, words []string, category string) bool {
	if category != "" && !strings.EqualFold(m.Category, category) {
		return false
	}
	text := strings.ToLower(m.Memory)
	for _, word := range words {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

func (d *Database) UpdateMemory(ctx context.Context, memory database.UserMemory) error {
	if memory.ID == "" {
		return database.ErrEmptyID
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	i := d.indexOf(memory.ID)
	if i < 0 {
		return fmt.Errorf("%w: %s", database.ErrMemoryNotFound, memory.ID)
	}
	d.memories[i].Memory = memory.Memory
	d.memories[i].Category = memory.Category
	return nil
}

// indexOf must be called with d.mu held.
func (d *Database) indexOf(id string) int {
	return slices.IndexFunc(d.memories, func(m database.UserMemory) bool {
		return m.ID == id
	})
}
