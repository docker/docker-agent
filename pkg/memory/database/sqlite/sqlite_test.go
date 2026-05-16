package sqlite

import (
	"context"
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
		// Close connection
		memDB := db.(*MemoryDatabase)
		memDB.db.Close()
	})

	return db
}

func TestNewMemoryDatabase(t *testing.T) {
	db := setupTestDB(t)

	assert.NotNil(t, db, "Database should be created successfully")

	_, err := NewMemoryDatabase("/:invalid:path")
	require.Error(t, err, "Should fail with invalid database path")
}

func TestAddMemory(t *testing.T) {
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
	memDB1.db.Close()

	// Reopen - migration should be idempotent
	db2, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	memDB2 := db2.(*MemoryDatabase)
	defer memDB2.db.Close()

	memories, err := db2.GetMemories(t.Context())
	require.NoError(t, err)
	require.Len(t, memories, 1)
	assert.Equal(t, "Old memory without category", memories[0].Memory)
	assert.Empty(t, memories[0].Category)
}

func TestDatabaseOperationsWithCanceledContext(t *testing.T) {
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

func TestDatabaseWithMultipleInstances(t *testing.T) {
	tmpFile := t.TempDir() + "/shared.db"
	db1, err := NewMemoryDatabase(tmpFile)
	require.NoError(t, err)
	defer func() {
		memDB := db1.(*MemoryDatabase)
		memDB.db.Close()
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
		memDB.db.Close()
	}()

	memories, err := db2.GetMemories(t.Context())
	require.NoError(t, err)
	assert.Len(t, memories, 1, "Second instance should see memory added by first instance")
	assert.Equal(t, "shared-id", memories[0].ID)
	assert.Equal(t, "Shared memory", memories[0].Memory)
}
