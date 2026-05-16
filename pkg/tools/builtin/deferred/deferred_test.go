package deferred

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type mockToolSet struct {
	toolList []tools.Tool
}

func (m *mockToolSet) Tools(_ context.Context) ([]tools.Tool, error) {
	return m.toolList, nil
}

func TestDeferredToolset_SearchTool(t *testing.T) {
	ctx := t.Context()
	mockTools := &mockToolSet{
		toolList: []tools.Tool{
			{Name: "create_file", Description: "Creates a new file"},
			{Name: "read_file", Description: "Reads file content"},
			{Name: "delete_file", Description: "Deletes a file"},
		},
	}

	dt := NewDeferredToolset()
	dt.AddSource(mockTools, true, nil)
	err := dt.Start(ctx)
	require.NoError(t, err)

	_, err = dt.Tools(ctx)
	require.NoError(t, err)

	t.Run("search by name", func(t *testing.T) {
		result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "create"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "create_file")
		assert.NotContains(t, result.Output, "read_file")
	})

	t.Run("search by description", func(t *testing.T) {
		result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "content"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "read_file")
	})

	t.Run("search no results", func(t *testing.T) {
		result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "nonexistent"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "No deferred tools found")
	})
}

func TestDeferredToolset_AddTool(t *testing.T) {
	ctx := t.Context()
	mockTools := &mockToolSet{
		toolList: []tools.Tool{
			{Name: "tool1", Description: "First tool"},
			{Name: "tool2", Description: "Second tool"},
		},
	}

	dt := NewDeferredToolset()
	dt.AddSource(mockTools, true, nil)
	err := dt.Start(ctx)
	require.NoError(t, err)

	initialTools, err := dt.Tools(ctx)
	require.NoError(t, err)
	assert.Len(t, initialTools, 2)
	t.Run("add existing deferred tool", func(t *testing.T) {
		result, err := dt.handleAddTool(ctx, AddToolArgs{Name: "tool1"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "has been activated")

		currentTools, err := dt.Tools(ctx)
		require.NoError(t, err)
		assert.Len(t, currentTools, 3) // search_tool, add_tool, tool1

		toolNames := make([]string, len(currentTools))
		for i, tool := range currentTools {
			toolNames[i] = tool.Name
		}
		assert.Contains(t, toolNames, "tool1")
	})

	t.Run("add already active tool", func(t *testing.T) {
		result, err := dt.handleAddTool(ctx, AddToolArgs{Name: "tool1"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "already active")
	})

	t.Run("add non-existent tool", func(t *testing.T) {
		result, err := dt.handleAddTool(ctx, AddToolArgs{Name: "nonexistent"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "not found")
	})
}
