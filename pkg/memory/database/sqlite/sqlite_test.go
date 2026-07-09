package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/memory/database"
)

func setupTestDB(t *testing.T) database.Database {
	t.Helper()

	tmpFile := t.TempDir() + "/test.db"

	db, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	require.NotNil(t, db)

	t.Cleanup(func() {
		memDB := db.(*MemoryDatabase)
		require.NoError(t, memDB.Close())
	})

	return db
}

func TestNewMemoryDatabase(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)

	assert.NotNil(t, db, "Database should be created successfully")

	db, err := NewMemoryDatabase("/:invalid:path")
	require.NoError(t, err, "constructor should not touch the filesystem")
	err = db.AddMemory(t.Context(), database.UserMemory{ID: "1", CreatedAt: time.Now().Format(time.RFC3339), Memory: "x"})
	require.Error(t, err, "Should fail with invalid database path")
}

func TestAddMemory(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)

	ctx := t.Context()

	memory := database.UserMemory{
		ID:        "test-id-1",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Test memory content",
	}

	err := db.AddMemory(ctx, memory)
	require.NoError(t, err, "Adding memory should succeed")

	err = db.AddMemory(ctx, memory)
	require.Error(t, err, "Adding memory with duplicate ID should fail")

	emptyIDMemory := database.UserMemory{
		ID:        "",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Empty ID memory",
	}

	err = db.AddMemory(ctx, emptyIDMemory)
	require.Error(t, err, "Adding memory with empty ID should fail")
}

func TestAddMemoryWithCategory(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)

	memory := database.UserMemory{
		ID:        "cat-1",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "User prefers dark mode",
		Category:  "preference",
	}

	err := db.AddMemory(t.Context(), memory)
	require.NoError(t, err)

	memories, err := db.GetMemories(t.Context())
	require.NoError(t, err)
	require.Len(t, memories, 1)
	assert.Equal(t, "preference", memories[0].Category)
}

func TestGetMemories(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)

	memories, err := db.GetMemories(t.Context())
	require.NoError(t, err)
	assert.Empty(t, memories, "Empty database should return empty memories slice")

	testMemories := []database.UserMemory{
		{
			ID:        "test-id-1",
			CreatedAt: time.Now().Format(time.RFC3339),
			Memory:    "First test memory",
		},
		{
			ID:        "test-id-2",
			CreatedAt: time.Now().Format(time.RFC3339),
			Memory:    "Second test memory",
		},
	}

	for _, memory := range testMemories {
		err := db.AddMemory(t.Context(), memory)
		require.NoError(t, err)
	}

	memories, err = db.GetMemories(t.Context())
	require.NoError(t, err)
	assert.Len(t, memories, 2, "Should retrieve both added memories")

	memoryMap := make(map[string]database.UserMemory)
	for _, memory := range memories {
		memoryMap[memory.ID] = memory
	}

	for _, expected := range testMemories {
		actual, exists := memoryMap[expected.ID]
		assert.True(t, exists, "Memory with ID %s should exist", expected.ID)
		assert.Equal(t, expected.Memory, actual.Memory)
		assert.Equal(t, expected.CreatedAt, actual.CreatedAt)
	}
}

func TestDeleteMemory(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)

	memory := database.UserMemory{
		ID:        "test-id-1",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Test memory to delete",
	}

	err := db.AddMemory(t.Context(), memory)
	require.NoError(t, err)

	memories, err := db.GetMemories(t.Context())
	require.NoError(t, err)
	require.Len(t, memories, 1)

	// Delete the memory
	err = db.DeleteMemory(t.Context(), memory)
	require.NoError(t, err, "Deleting existing memory should succeed")

	memories, err = db.GetMemories(t.Context())
	require.NoError(t, err)
	assert.Empty(t, memories, "Memory should be deleted")

	// Try deleting non-existent memory
	nonExistentMemory := database.UserMemory{
		ID: "non-existent-id",
	}
	err = db.DeleteMemory(t.Context(), nonExistentMemory)
	require.NoError(t, err, "Deleting non-existent memory should not return an error")
}

func TestSearchMemories(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := t.Context()

	testMemories := []database.UserMemory{
		{ID: "1", CreatedAt: time.Now().Format(time.RFC3339), Memory: "User prefers dark mode", Category: "preference"},
		{ID: "2", CreatedAt: time.Now().Format(time.RFC3339), Memory: "Project uses Go and React", Category: "project"},
		{ID: "3", CreatedAt: time.Now().Format(time.RFC3339), Memory: "User likes Go for backend", Category: "preference"},
		{ID: "4", CreatedAt: time.Now().Format(time.RFC3339), Memory: "Deploy to AWS us-east-1", Category: "project"},
	}
	for _, m := range testMemories {
		require.NoError(t, db.AddMemory(ctx, m))
	}

	t.Run("single keyword", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "Go", "")
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})

	t.Run("multi-word AND", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "Go backend", "")
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "3", results[0].ID)
	})

	t.Run("category filter only", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "", "preference")
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})

	t.Run("keyword plus category", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "Go", "project")
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "2", results[0].ID)
	})

	t.Run("empty query returns all", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "", "")
		require.NoError(t, err)
		assert.Len(t, results, 4)
	})

	t.Run("no matches", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "nonexistent", "")
		require.NoError(t, err)
		assert.Empty(t, results)
	})

	t.Run("case insensitive", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "go", "")
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})

	t.Run("case insensitive category", func(t *testing.T) {
		results, err := db.SearchMemories(ctx, "", "PREFERENCE")
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})
}

func TestUpdateMemory(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := t.Context()

	memory := database.UserMemory{
		ID:        "upd-1",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Original content",
		Category:  "fact",
	}
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
		// CreatedAt should be preserved
		assert.Equal(t, memory.CreatedAt, memories[0].CreatedAt)
	})

	t.Run("not found", func(t *testing.T) {
		err := db.UpdateMemory(ctx, database.UserMemory{
			ID:     "nonexistent",
			Memory: "something",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, database.ErrMemoryNotFound)
	})

	t.Run("empty ID", func(t *testing.T) {
		err := db.UpdateMemory(ctx, database.UserMemory{
			ID:     "",
			Memory: "something",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, database.ErrEmptyID)
	})
}

func TestMigrationAddsCategory(t *testing.T) {
	t.Parallel()
	tmpFile := t.TempDir() + "/migrate.db"

	// Create a DB with the old schema (no category column)
	db1, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	memDB1 := db1.(*MemoryDatabase)

	// Add a memory (which now includes category column from migration)
	err = db1.AddMemory(t.Context(), database.UserMemory{
		ID:        "old-1",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Old memory without category",
	})
	require.NoError(t, err)
	require.NoError(t, memDB1.Close())

	// Reopen - migration should be idempotent
	db2, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	memDB2 := db2.(*MemoryDatabase)
	defer func() { require.NoError(t, memDB2.Close()) }()

	memories, err := db2.GetMemories(t.Context())
	require.NoError(t, err)
	require.Len(t, memories, 1)
	assert.Equal(t, "Old memory without category", memories[0].Memory)
	assert.Empty(t, memories[0].Category)
}

func TestDatabaseOperationsWithCanceledContext(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	memory := database.UserMemory{
		ID:        "test-id",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Test memory",
	}

	err := db.AddMemory(ctx, memory)
	require.Error(t, err, "AddMemory should fail with canceled context")

	_, err = db.GetMemories(ctx)
	require.Error(t, err, "GetMemories should fail with canceled context")

	err = db.DeleteMemory(ctx, memory)
	require.Error(t, err, "DeleteMemory should fail with canceled context")

	_, err = db.SearchMemories(ctx, "test", "")
	require.Error(t, err, "SearchMemories should fail with canceled context")

	err = db.UpdateMemory(ctx, memory)
	require.Error(t, err, "UpdateMemory should fail with canceled context")
}

func TestMemoryDatabaseUsesWALAndBusyTimeout(t *testing.T) {
	// WAL is only expected outside a Docker sandbox (sqliteutil falls back
	// to DELETE inside one), so pin the detection env var.
	t.Setenv("SANDBOX_VM_ID", "")

	db := setupTestDB(t)
	memDB := db.(*MemoryDatabase)

	sqlDB, err := memDB.ensureDB(t.Context())
	require.NoError(t, err)

	var journalMode string
	require.NoError(t, sqlDB.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)

	var busyTimeout int
	require.NoError(t, sqlDB.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&busyTimeout))
	assert.Equal(t, 5000, busyTimeout)
}

func TestDatabaseWithMultipleInstances(t *testing.T) {
	t.Parallel()
	tmpFile := t.TempDir() + "/shared.db"
	db1, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	defer func() {
		memDB := db1.(*MemoryDatabase)
		require.NoError(t, memDB.Close())
	}()

	memory := database.UserMemory{
		ID:        "shared-id",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "Shared memory",
	}

	err = db1.AddMemory(t.Context(), memory)
	require.NoError(t, err)

	db2, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	defer func() {
		memDB := db2.(*MemoryDatabase)
		require.NoError(t, memDB.Close())
	}()

	memories, err := db2.GetMemories(t.Context())
	require.NoError(t, err)
	assert.Len(t, memories, 1, "Second instance should see memory added by first instance")
	assert.Equal(t, "shared-id", memories[0].ID)
	assert.Equal(t, "Shared memory", memories[0].Memory)
}

func TestConcurrentAddsPreserveAllRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")
	const workers = 8
	const perWorker = 25

	dbs := make([]database.Database, workers)
	for i := range workers {
		db, err := NewMemoryDatabase(dbPath)
		require.NoError(t, err)
		dbs[i] = db
		memDB := db.(*MemoryDatabase)
		t.Cleanup(func() { _ = memDB.Close() })
	}

	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for i := range perWorker {
				id := fmt.Sprintf("worker-%d-%d", worker, i)
				require.NoError(t, dbs[worker].AddMemory(t.Context(), database.UserMemory{
					ID:        id,
					CreatedAt: time.Now().Format(time.RFC3339),
					Memory:    "concurrent add",
				}))
			}
		})
	}
	wg.Wait()

	reader, err := NewMemoryDatabase(dbPath)
	require.NoError(t, err)
	readerDB := reader.(*MemoryDatabase)
	defer func() { _ = readerDB.Close() }()

	memories, err := reader.GetMemories(t.Context())
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
	dbPath := filepath.Join(t.TempDir(), "reads-writes.db")
	db, err := NewMemoryDatabase(dbPath)
	require.NoError(t, err)
	memDB := db.(*MemoryDatabase)
	defer func() { _ = memDB.Close() }()

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
					readErr <- errors.New("read malformed memory with empty ID")
					return
				}
			}
		}
	}()

	for i := range 100 {
		id := fmt.Sprintf("rw-%d", i)
		require.NoError(t, db.AddMemory(ctx, database.UserMemory{
			ID:        id,
			CreatedAt: time.Now().Format(time.RFC3339),
			Memory:    "initial",
		}))
		require.NoError(t, db.UpdateMemory(ctx, database.UserMemory{
			ID:     id,
			Memory: "updated",
		}))
		if i%3 == 0 {
			require.NoError(t, db.DeleteMemory(ctx, database.UserMemory{ID: id}))
		}
	}
	close(done)
	if err := <-readErr; err != nil {
		require.NoError(t, err)
	}
}

func TestWriteCreatesPersistentLockFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "locked.db")
	db, err := NewMemoryDatabase(dbPath)
	require.NoError(t, err)
	memDB := db.(*MemoryDatabase)
	defer func() { _ = memDB.Close() }()

	lockPath := database.LockPathForDatabase(dbPath)

	require.NoError(t, db.AddMemory(t.Context(), database.UserMemory{
		ID:        "lock-file",
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    "creates lock",
	}))
	require.FileExists(t, lockPath)

	require.NoError(t, db.UpdateMemory(t.Context(), database.UserMemory{
		ID:     "lock-file",
		Memory: "preserves lock",
	}))
	require.FileExists(t, lockPath)
}
