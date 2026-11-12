package inmemory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/memory/database"
)

func newMemory(id, content, category string) database.UserMemory {
	return database.UserMemory{
		ID:        id,
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    content,
		Category:  category,
	}
}

func TestAddMemory(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()

	memory := newMemory("test-id-1", "Test memory content", "")
	require.NoError(t, db.AddMemory(ctx, memory))

	err := db.AddMemory(ctx, memory)
	require.ErrorIs(t, err, ErrDuplicateID)

	err = db.AddMemory(ctx, newMemory("", "Empty ID memory", ""))
	require.ErrorIs(t, err, database.ErrEmptyID)

	memories, err := db.GetMemories(ctx)
	require.NoError(t, err)
	require.Len(t, memories, 1)
	assert.Equal(t, memory, memories[0])
}

func TestGetMemories(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()

	memories, err := db.GetMemories(ctx)
	require.NoError(t, err)
	assert.Nil(t, memories, "empty database returns a nil slice like sqlite")

	first := newMemory("test-id-1", "First test memory", "")
	second := newMemory("test-id-2", "Second test memory", "preference")
	require.NoError(t, db.AddMemory(ctx, first))
	require.NoError(t, db.AddMemory(ctx, second))

	memories, err = db.GetMemories(ctx)
	require.NoError(t, err)
	assert.Equal(t, []database.UserMemory{first, second}, memories, "insertion order is preserved")

	memories[0].Memory = "mutated"
	memories, err = db.GetMemories(ctx)
	require.NoError(t, err)
	assert.Equal(t, "First test memory", memories[0].Memory, "callers get a copy")
}

func TestDeleteMemory(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()

	memory := newMemory("test-id-1", "Test memory to delete", "")
	require.NoError(t, db.AddMemory(ctx, memory))

	require.NoError(t, db.DeleteMemory(ctx, memory))

	memories, err := db.GetMemories(ctx)
	require.NoError(t, err)
	assert.Empty(t, memories)

	require.NoError(t, db.DeleteMemory(ctx, database.UserMemory{ID: "non-existent-id"}),
		"deleting a non-existent memory is not an error")
}

func TestSearchMemories(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()

	for _, m := range []database.UserMemory{
		newMemory("1", "User prefers dark mode", "preference"),
		newMemory("2", "Project uses Go and React", "project"),
		newMemory("3", "User likes Go for backend", "preference"),
		newMemory("4", "Deploy to AWS us-east-1", "project"),
		newMemory("5", "Progress is 100% done_now", "project"),
	} {
		require.NoError(t, db.AddMemory(ctx, m))
	}

	tests := []struct {
		name     string
		query    string
		category string
		wantIDs  []string
	}{
		{name: "single keyword", query: "Go", wantIDs: []string{"2", "3"}},
		{name: "multi-word AND", query: "Go backend", wantIDs: []string{"3"}},
		{name: "category filter only", category: "preference", wantIDs: []string{"1", "3"}},
		{name: "keyword plus category", query: "Go", category: "project", wantIDs: []string{"2"}},
		{name: "empty query returns all", wantIDs: []string{"1", "2", "3", "4", "5"}},
		{name: "whitespace query returns all", query: "  \t ", wantIDs: []string{"1", "2", "3", "4", "5"}},
		{name: "no matches", query: "nonexistent", wantIDs: nil},
		{name: "case insensitive", query: "go", wantIDs: []string{"2", "3"}},
		{name: "case insensitive category", category: "PREFERENCE", wantIDs: []string{"1", "3"}},
		{name: "unknown category", category: "nope", wantIDs: nil},
		{name: "LIKE wildcards are literal", query: "100%", wantIDs: []string{"5"}},
		{name: "underscore is literal", query: "done_now", wantIDs: []string{"5"}},
		{name: "underscore does not match any char", query: "donexnow", wantIDs: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			results, err := db.SearchMemories(ctx, tt.query, tt.category)
			require.NoError(t, err)

			var ids []string
			for _, m := range results {
				ids = append(ids, m.ID)
			}
			assert.Equal(t, tt.wantIDs, ids)
		})
	}
}

func TestUpdateMemory(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()

	memory := newMemory("upd-1", "Original content", "fact")
	require.NoError(t, db.AddMemory(ctx, memory))

	t.Run("update content and category", func(t *testing.T) {
		err := db.UpdateMemory(ctx, database.UserMemory{
			ID:       "upd-1",
			Memory:   "Updated content",
			Category: "decision",
		})
		require.NoError(t, err)

		memories, err := db.GetMemories(ctx)
		require.NoError(t, err)
		require.Len(t, memories, 1)
		assert.Equal(t, "Updated content", memories[0].Memory)
		assert.Equal(t, "decision", memories[0].Category)
		assert.Equal(t, memory.CreatedAt, memories[0].CreatedAt, "CreatedAt is preserved")
	})

	t.Run("not found", func(t *testing.T) {
		err := db.UpdateMemory(ctx, database.UserMemory{ID: "nonexistent", Memory: "something"})
		require.ErrorIs(t, err, database.ErrMemoryNotFound)
		assert.ErrorContains(t, err, "nonexistent")
	})

	t.Run("empty ID", func(t *testing.T) {
		err := db.UpdateMemory(ctx, database.UserMemory{ID: "", Memory: "something"})
		require.ErrorIs(t, err, database.ErrEmptyID)
	})
}

func TestDatabaseOperationsWithCanceledContext(t *testing.T) {
	t.Parallel()
	db := New()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	memory := newMemory("test-id", "Test memory", "")

	require.ErrorIs(t, db.AddMemory(ctx, memory), context.Canceled)

	_, err := db.GetMemories(ctx)
	require.ErrorIs(t, err, context.Canceled)

	require.ErrorIs(t, db.DeleteMemory(ctx, memory), context.Canceled)

	_, err = db.SearchMemories(ctx, "test", "")
	require.ErrorIs(t, err, context.Canceled)

	require.ErrorIs(t, db.UpdateMemory(ctx, memory), context.Canceled)

	memories, err := db.GetMemories(t.Context())
	require.NoError(t, err)
	assert.Empty(t, memories, "canceled operations must not mutate the store")
}

func TestConcurrentAddsPreserveAllRows(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()
	const workers = 8
	const perWorker = 25

	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for i := range perWorker {
				id := fmt.Sprintf("worker-%d-%d", worker, i)
				assert.NoError(t, db.AddMemory(ctx, newMemory(id, "concurrent add", "")))
			}
		})
	}
	wg.Wait()

	memories, err := db.GetMemories(ctx)
	require.NoError(t, err)
	require.Len(t, memories, workers*perWorker)

	seen := make(map[string]bool, len(memories))
	for _, memory := range memories {
		seen[memory.ID] = true
	}
	for worker := range workers {
		for i := range perWorker {
			assert.True(t, seen[fmt.Sprintf("worker-%d-%d", worker, i)])
		}
	}
}

func TestConcurrentReadsDuringWrites(t *testing.T) {
	t.Parallel()
	db := New()
	ctx := t.Context()

	done := make(chan struct{})
	readErr := make(chan error, 1)

	go func() {
		defer close(readErr)
		for {
			select {
			case <-done:
				return
			default:
			}
			memories, err := db.GetMemories(ctx)
			if err != nil {
				readErr <- err
				return
			}
			for _, memory := range memories {
				if memory.ID == "" {
					readErr <- fmt.Errorf("read malformed memory: %+v", memory)
					return
				}
			}
		}
	}()

	for i := range 100 {
		id := fmt.Sprintf("rw-%d", i)
		require.NoError(t, db.AddMemory(ctx, newMemory(id, "initial", "")))
		require.NoError(t, db.UpdateMemory(ctx, database.UserMemory{ID: id, Memory: "updated"}))
		if i%3 == 0 {
			require.NoError(t, db.DeleteMemory(ctx, database.UserMemory{ID: id}))
		}
	}
	close(done)
	require.NoError(t, <-readErr)
}
