package memory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/memory/database"
	"github.com/docker/docker-agent/pkg/tools"
)

// Mock for memorymanager.Manager
type MockDB struct {
	mock.Mock
}

func (m *MockDB) AddMemory(ctx context.Context, memory database.UserMemory) error {
	args := m.Called(ctx, memory)
	return args.Error(0)
}

func (m *MockDB) GetMemories(ctx context.Context) ([]database.UserMemory, error) {
	args := m.Called(ctx)
	return args.Get(0).([]database.UserMemory), args.Error(1)
}

func (m *MockDB) DeleteMemory(ctx context.Context, memory database.UserMemory) error {
	args := m.Called(ctx, memory)
	return args.Error(0)
}

func (m *MockDB) SearchMemories(ctx context.Context, query, category string) ([]database.UserMemory, error) {
	args := m.Called(ctx, query, category)
	return args.Get(0).([]database.UserMemory), args.Error(1)
}

func (m *MockDB) UpdateMemory(ctx context.Context, memory database.UserMemory) error {
	args := m.Called(ctx, memory)
	return args.Error(0)
}

func TestMemoryTool_Instructions(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	instructions := tool.Instructions()
	assert.Contains(t, instructions, "Memory Tools")
	assert.Contains(t, instructions, "search_memories")
	assert.Contains(t, instructions, "update_memory")
	assert.Contains(t, instructions, "preference")
}

func TestMemoryTool_DisplayNames(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range all {
		assert.NotEmpty(t, tool.DisplayName())
		assert.NotEqual(t, tool.Name, tool.DisplayName())
	}
}

func TestMemoryTool_HandleAddMemory(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	manager.On("AddMemory", mock.Anything, mock.MatchedBy(func(memory database.UserMemory) bool {
		return memory.Memory == "test memory"
	})).Return(nil)

	result, err := tool.handleAddMemory(t.Context(), AddMemoryArgs{
		Memory: "test memory",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Memory added successfully")
	manager.AssertExpectations(t)
}

func TestMemoryTool_HandleAddMemoryWithCategory(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	manager.On("AddMemory", mock.Anything, mock.MatchedBy(func(memory database.UserMemory) bool {
		return memory.Memory == "prefers dark mode" && memory.Category == "preference"
	})).Return(nil)

	result, err := tool.handleAddMemory(t.Context(), AddMemoryArgs{
		Memory:   "prefers dark mode",
		Category: "preference",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Memory added successfully")
	manager.AssertExpectations(t)
}

func TestMemoryTool_HandleGetMemories(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	memories := []database.UserMemory{
		{
			ID:        "1",
			CreatedAt: time.Now().Format(time.RFC3339),
			Memory:    "memory 1",
		},
		{
			ID:        "2",
			CreatedAt: time.Now().Format(time.RFC3339),
			Memory:    "memory 2",
		},
	}
	manager.On("GetMemories", mock.Anything).Return(memories, nil)

	result, err := tool.handleGetMemories(t.Context(), nil)
	require.NoError(t, err)

	var returnedMemories []database.UserMemory
	err = json.Unmarshal([]byte(result.Output), &returnedMemories)
	require.NoError(t, err)

	assert.Len(t, returnedMemories, 2)
	assert.Equal(t, memories, returnedMemories)
	manager.AssertExpectations(t)
}

func TestMemoryTool_HandleDeleteMemory(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	manager.On("DeleteMemory", mock.Anything, mock.MatchedBy(func(memory database.UserMemory) bool {
		return memory.ID == "1"
	})).Return(nil)

	result, err := tool.handleDeleteMemory(t.Context(), DeleteMemoryArgs{
		ID: "1",
	})

	require.NoError(t, err)
	assert.Contains(t, result.Output, "Memory with ID 1 deleted successfully")
	manager.AssertExpectations(t)
}

func TestMemoryTool_HandleSearchMemories(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	memories := []database.UserMemory{
		{
			ID:        "1",
			CreatedAt: time.Now().Format(time.RFC3339),
			Memory:    "User prefers dark mode",
			Category:  "preference",
		},
	}
	manager.On("SearchMemories", mock.Anything, "dark mode", "preference").Return(memories, nil)

	result, err := tool.handleSearchMemories(t.Context(), SearchMemoriesArgs{
		Query:    "dark mode",
		Category: "preference",
	})
	require.NoError(t, err)

	var returnedMemories []database.UserMemory
	err = json.Unmarshal([]byte(result.Output), &returnedMemories)
	require.NoError(t, err)

	assert.Len(t, returnedMemories, 1)
	assert.Equal(t, "User prefers dark mode", returnedMemories[0].Memory)
	manager.AssertExpectations(t)
}

func TestMemoryTool_HandleUpdateMemory(t *testing.T) {
	manager := new(MockDB)
	tool := NewMemoryTool(manager)

	manager.On("UpdateMemory", mock.Anything, mock.MatchedBy(func(memory database.UserMemory) bool {
		return memory.ID == "42" && memory.Memory == "updated content" && memory.Category == "fact"
	})).Return(nil)

	result, err := tool.handleUpdateMemory(t.Context(), UpdateMemoryArgs{
		ID:       "42",
		Memory:   "updated content",
		Category: "fact",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Memory with ID 42 updated successfully")
	manager.AssertExpectations(t)
}

func TestMemoryTool_ToolCount(t *testing.T) {
	tool := NewMemoryTool(nil)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, 5, "Should have 5 tools: add, get, delete, search, update")
}

func TestMemoryTool_OutputSchema(t *testing.T) {
	tool := NewMemoryTool(nil)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestMemoryTool_ParametersAreObjects(t *testing.T) {
	tool := NewMemoryTool(nil)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)

		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}
