package deferred

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

type mockToolSet struct {
	toolList []tools.Tool
	// notStarted makes Tools fail like an MCP toolset that is still starting.
	notStarted atomic.Bool
	calls      atomic.Int32
}

func (m *mockToolSet) Tools(_ context.Context) ([]tools.Tool, error) {
	m.calls.Add(1)
	if m.notStarted.Load() {
		return nil, lifecycle.ErrNotStarted
	}
	return m.toolList, nil
}

func TestDeferredToolset_SearchTool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	mockTools := &mockToolSet{
		toolList: []tools.Tool{
			{Name: "create_file", Description: "Creates a new file"},
			{Name: "read_file", Description: "Reads file content"},
			{Name: "delete_file", Description: "Deletes a file"},
		},
	}

	dt := New()
	dt.AddSource(mockTools, true, nil)

	_, err := dt.Tools(ctx)
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

	t.Run("fuzzy search by name", func(t *testing.T) {
		result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "crfil"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "create_file")
		assert.NotContains(t, result.Output, "read_file")
	})

	t.Run("fuzzy search by description", func(t *testing.T) {
		result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "dfle"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "delete_file")
	})
}

func TestDeferredToolset_AddTool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	mockTools := &mockToolSet{
		toolList: []tools.Tool{
			{Name: "tool1", Description: "First tool"},
			{Name: "tool2", Description: "Second tool"},
		},
	}

	dt := New()
	dt.AddSource(mockTools, true, nil)

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

	t.Run("activated tool no longer listed by search", func(t *testing.T) {
		result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "tool"})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "tool2")
		assert.NotContains(t, result.Output, "tool1")
	})
}

func TestDeferredToolset_PartialDefer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	mockTools := &mockToolSet{
		toolList: []tools.Tool{
			{Name: "kept", Description: "Always exposed"},
			{Name: "deferred", Description: "Only on demand"},
		},
	}

	dt := New()
	dt.AddSource(mockTools, false, []string{"deferred"})

	result, err := dt.handleSearchTool(ctx, SearchToolArgs{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "deferred")
	assert.NotContains(t, result.Output, "kept")

	result, err = dt.handleAddTool(ctx, AddToolArgs{Name: "kept"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "not found")
}

// A source that has not finished starting (e.g. an MCP server still coming up
// when the first turn runs) must not break the deferred toolset: its tools are
// simply absent until it is ready, while other sources stay searchable.
func TestDeferredToolset_SourceNotStartedYet(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	slow := &mockToolSet{toolList: []tools.Tool{{Name: "slow_tool", Description: "From the slow source"}}}
	slow.notStarted.Store(true)
	ready := &mockToolSet{toolList: []tools.Tool{{Name: "ready_tool", Description: "From the ready source"}}}

	dt := New()
	dt.AddSource(slow, true, nil)
	dt.AddSource(ready, true, nil)

	result, err := dt.handleSearchTool(ctx, SearchToolArgs{Query: "tool"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "ready_tool")
	assert.NotContains(t, result.Output, "slow_tool")

	result, err = dt.handleAddTool(ctx, AddToolArgs{Name: "slow_tool"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "not found")

	slow.notStarted.Store(false)

	result, err = dt.handleSearchTool(ctx, SearchToolArgs{Query: "slow"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "slow_tool")

	result, err = dt.handleAddTool(ctx, AddToolArgs{Name: "slow_tool"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "has been activated")

	currentTools, err := dt.Tools(ctx)
	require.NoError(t, err)
	assert.Len(t, currentTools, 3) // search_tool, add_tool, slow_tool

	// Each source is listed until it succeeds, then snapshotted: slow failed
	// twice then succeeded once; ready succeeded on the first call.
	assert.Equal(t, int32(3), slow.calls.Load())
	assert.Equal(t, int32(1), ready.calls.Load())
}

func TestDeferredToolset_FirstSourceWinsOnDuplicateName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	first := &mockToolSet{toolList: []tools.Tool{{Name: "dup", Description: "from first"}}}
	second := &mockToolSet{toolList: []tools.Tool{{Name: "dup", Description: "from second"}}}

	dt := New()
	dt.AddSource(first, true, nil)
	dt.AddSource(second, true, nil)

	result, err := dt.handleAddTool(ctx, AddToolArgs{Name: "dup"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "from first")
}

func TestDeferredToolset_ConcurrentAddTool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	source := &mockToolSet{toolList: []tools.Tool{{Name: "tool1", Description: "First tool"}}}
	dt := New()
	dt.AddSource(source, true, nil)

	var activated, alreadyActive atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			result, err := dt.handleAddTool(ctx, AddToolArgs{Name: "tool1"})
			if err != nil {
				t.Error(err)
				return
			}
			switch {
			case strings.Contains(result.Output, "has been activated"):
				activated.Add(1)
			case strings.Contains(result.Output, "already active"):
				alreadyActive.Add(1)
			default:
				t.Errorf("unexpected outcome: %s", result.Output)
			}
		})
	}
	wg.Wait()

	assert.Equal(t, int32(1), activated.Load(), "exactly one call must report the activation")
	assert.Equal(t, int32(15), alreadyActive.Load())
	currentTools, err := dt.Tools(ctx)
	require.NoError(t, err)
	assert.Len(t, currentTools, 3) // search_tool, add_tool, tool1 — never duplicated
}
