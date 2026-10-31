package todo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestTodoTool_DisplayNames(t *testing.T) {
	t.Parallel()
	tool := New()

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range all {
		assert.NotEmpty(t, tool.DisplayName())
		assert.NotEqual(t, tool.Name, tool.DisplayName())
	}
}

func TestTodoTool_CreateTodo(t *testing.T) {
	t.Parallel()
	storage := NewMemoryTodoStorage()
	tool := New(WithStorage(storage))

	result, err := tool.handler.createTodo(t.Context(), CreateTodoArgs{
		Description: "Test todo item",
	})
	require.NoError(t, err)

	var output CreateTodoOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	assert.Equal(t, "todo_1", output.Created.ID)
	assert.Equal(t, "Test todo item", output.Created.Description)
	assert.Equal(t, "pending", output.Created.Status)

	// Full state is included in the response
	require.Len(t, output.AllTodos, 1)
	assert.Equal(t, "todo_1", output.AllTodos[0].ID)
	assert.Contains(t, output.Reminder, "todo_1")

	require.Equal(t, 1, storage.Len(t.Context()))
	requireMeta(t, result, 1)
}

func TestTodoTool_CreateTodos(t *testing.T) {
	t.Parallel()
	storage := NewMemoryTodoStorage()
	tool := New(WithStorage(storage))

	result, err := tool.handler.createTodos(t.Context(), CreateTodosArgs{
		Descriptions: []string{"First", "Second", "Third"},
	})
	require.NoError(t, err)

	var output CreateTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	require.Len(t, output.Created, 3)
	assert.Equal(t, "todo_1", output.Created[0].ID)
	assert.Equal(t, "First", output.Created[0].Description)
	assert.Equal(t, "pending", output.Created[0].Status)
	assert.Equal(t, "todo_2", output.Created[1].ID)
	assert.Equal(t, "todo_3", output.Created[2].ID)

	// Full state included in response
	require.Len(t, output.AllTodos, 3)
	assert.Contains(t, output.Reminder, "todo_1")
	assert.Contains(t, output.Reminder, "todo_2")
	assert.Contains(t, output.Reminder, "todo_3")

	assert.Equal(t, 3, storage.Len(t.Context()))
	requireMeta(t, result, 3)

	// A second call continues the ID sequence and includes all 4 items
	result, err = tool.handler.createTodos(t.Context(), CreateTodosArgs{
		Descriptions: []string{"Last"},
	})
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	require.Len(t, output.Created, 1)
	assert.Equal(t, "todo_4", output.Created[0].ID)
	require.Len(t, output.AllTodos, 4)
	assert.Equal(t, 4, storage.Len(t.Context()))
	requireMeta(t, result, 4)
}

func TestTodoTool_ListTodos(t *testing.T) {
	t.Parallel()
	tool := New()

	descs := []string{"First", "Second", "Third"}
	for _, d := range descs {
		_, err := tool.handler.createTodo(t.Context(), CreateTodoArgs{Description: d})
		require.NoError(t, err)
	}

	result, err := tool.handler.listTodos(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)

	var output ListTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	require.Len(t, output.Todos, 3)
	for i, d := range descs {
		assert.Equal(t, d, output.Todos[i].Description)
		assert.Equal(t, "pending", output.Todos[i].Status)
	}

	// All pending, so reminder should list all of them
	assert.Contains(t, output.Reminder, "todo_1")
	assert.Contains(t, output.Reminder, "todo_2")
	assert.Contains(t, output.Reminder, "todo_3")

	requireMeta(t, result, 3)
}

func TestTodoTool_ListTodos_Empty(t *testing.T) {
	t.Parallel()
	tool := New()

	result, err := tool.handler.listTodos(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)

	var output ListTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	assert.Empty(t, output.Todos)
	assert.Empty(t, output.Reminder)

	requireMeta(t, result, 0)
}

func TestTodoTool_UpdateTodos(t *testing.T) {
	t.Parallel()
	storage := NewMemoryTodoStorage()
	tool := New(WithStorage(storage))

	_, err := tool.handler.createTodos(t.Context(), CreateTodosArgs{
		Descriptions: []string{"First", "Second", "Third"},
	})
	require.NoError(t, err)

	result, err := tool.handler.updateTodos(t.Context(), UpdateTodosArgs{
		Updates: []Update{
			{ID: "todo_1", Status: "completed"},
			{ID: "todo_3", Status: "in-progress"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output UpdateTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	require.Len(t, output.Updated, 2)
	assert.Equal(t, "todo_1", output.Updated[0].ID)
	assert.Equal(t, "completed", output.Updated[0].Status)
	assert.Equal(t, "todo_3", output.Updated[1].ID)
	assert.Equal(t, "in-progress", output.Updated[1].Status)
	assert.Empty(t, output.NotFound)

	// Full state included in response
	require.Len(t, output.AllTodos, 3)
	assert.Equal(t, "completed", output.AllTodos[0].Status)
	assert.Equal(t, "pending", output.AllTodos[1].Status)
	assert.Equal(t, "in-progress", output.AllTodos[2].Status)

	// Reminder should list incomplete todos
	assert.Contains(t, output.Reminder, "todo_2")
	assert.Contains(t, output.Reminder, "todo_3")
	assert.NotContains(t, output.Reminder, "todo_1") // completed, should not appear

	todos := storage.All(t.Context())
	require.Len(t, todos, 3)
	assert.Equal(t, "completed", todos[0].Status)
	assert.Equal(t, "pending", todos[1].Status)
	assert.Equal(t, "in-progress", todos[2].Status)

	requireMeta(t, result, 3)
}

func TestTodoTool_UpdateTodos_PartialFailure(t *testing.T) {
	t.Parallel()
	storage := NewMemoryTodoStorage()
	tool := New(WithStorage(storage))

	_, err := tool.handler.createTodos(t.Context(), CreateTodosArgs{
		Descriptions: []string{"First", "Second"},
	})
	require.NoError(t, err)

	result, err := tool.handler.updateTodos(t.Context(), UpdateTodosArgs{
		Updates: []Update{
			{ID: "todo_1", Status: "completed"},
			{ID: "nonexistent", Status: "completed"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var output UpdateTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	require.Len(t, output.Updated, 1)
	assert.Equal(t, "todo_1", output.Updated[0].ID)
	require.Len(t, output.NotFound, 1)
	assert.Equal(t, "nonexistent", output.NotFound[0])

	// Reminder should mention the still-pending todo
	assert.Contains(t, output.Reminder, "todo_2")

	todos := storage.All(t.Context())
	require.Len(t, todos, 2)
	assert.Equal(t, "completed", todos[0].Status)
	assert.Equal(t, "pending", todos[1].Status)
}

func TestTodoTool_UpdateTodos_AllNotFound(t *testing.T) {
	t.Parallel()
	tool := New()

	result, err := tool.handler.updateTodos(t.Context(), UpdateTodosArgs{
		Updates: []Update{
			{ID: "nonexistent1", Status: "completed"},
			{ID: "nonexistent2", Status: "completed"},
		},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	var output UpdateTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	assert.Empty(t, output.Updated)
	require.Len(t, output.NotFound, 2)
	assert.Equal(t, "nonexistent1", output.NotFound[0])
	assert.Equal(t, "nonexistent2", output.NotFound[1])
}

func TestTodoTool_UpdateTodos_AllCompleted_NoAutoRemoval(t *testing.T) {
	t.Parallel()
	storage := NewMemoryTodoStorage()
	tool := New(WithStorage(storage))

	_, err := tool.handler.createTodos(t.Context(), CreateTodosArgs{
		Descriptions: []string{"First", "Second"},
	})
	require.NoError(t, err)

	result, err := tool.handler.updateTodos(t.Context(), UpdateTodosArgs{
		Updates: []Update{
			{ID: "todo_1", Status: "completed"},
			{ID: "todo_2", Status: "completed"},
		},
	})
	require.NoError(t, err)

	var output UpdateTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	require.Len(t, output.Updated, 2)
	assert.Empty(t, output.Reminder) // no reminder when all completed

	// Full state shows both items as completed
	require.Len(t, output.AllTodos, 2)
	assert.Equal(t, "completed", output.AllTodos[0].Status)
	assert.Equal(t, "completed", output.AllTodos[1].Status)

	// Todos remain in storage (no auto-clear on completion)
	assert.Equal(t, 2, storage.Len(t.Context()))
	requireMeta(t, result, 2)
}

func TestTodoTool_WithStorage(t *testing.T) {
	t.Parallel()
	storage := NewMemoryTodoStorage()
	tool := New(WithStorage(storage))

	_, err := tool.handler.createTodo(t.Context(), CreateTodoArgs{Description: "Test item"})
	require.NoError(t, err)

	assert.Equal(t, 1, storage.Len(t.Context()))
	assert.Equal(t, "Test item", storage.All(t.Context())[0].Description)
}

func TestTodoTool_WithStorage_NilPanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		WithStorage(nil)
	})
}

func TestTodoTool_OutputSchema(t *testing.T) {
	t.Parallel()
	tool := New()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestTodoTool_ParametersAreObjects(t *testing.T) {
	t.Parallel()
	tool := New()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)

		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

func TestTodoTool_CreateTodo_FullStateOutput(t *testing.T) {
	t.Parallel()
	tool := New()

	// Create first todo
	result1, err := tool.handler.createTodo(t.Context(), CreateTodoArgs{Description: "First"})
	require.NoError(t, err)
	var out1 CreateTodoOutput
	require.NoError(t, json.Unmarshal([]byte(result1.Output), &out1))
	require.Len(t, out1.AllTodos, 1)
	assert.Contains(t, out1.Reminder, "todo_1")

	// Create second todo — response shows both
	result2, err := tool.handler.createTodo(t.Context(), CreateTodoArgs{Description: "Second"})
	require.NoError(t, err)
	var out2 CreateTodoOutput
	require.NoError(t, json.Unmarshal([]byte(result2.Output), &out2))
	require.Len(t, out2.AllTodos, 2)
	assert.Contains(t, out2.Reminder, "todo_1")
	assert.Contains(t, out2.Reminder, "todo_2")
}

func TestTodoTool_UpdateTodos_FullStateOutput(t *testing.T) {
	t.Parallel()
	tool := New()

	_, err := tool.handler.createTodos(t.Context(), CreateTodosArgs{
		Descriptions: []string{"A", "B", "C"},
	})
	require.NoError(t, err)

	result, err := tool.handler.updateTodos(t.Context(), UpdateTodosArgs{
		Updates: []Update{{ID: "todo_1", Status: "completed"}},
	})
	require.NoError(t, err)

	var output UpdateTodosOutput
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))

	// AllTodos shows full state including the completed item
	require.Len(t, output.AllTodos, 3)
	assert.Equal(t, "completed", output.AllTodos[0].Status)
	assert.Equal(t, "pending", output.AllTodos[1].Status)
	assert.Equal(t, "pending", output.AllTodos[2].Status)

	// Reminder only lists incomplete items
	assert.NotContains(t, output.Reminder, "todo_1")
	assert.Contains(t, output.Reminder, "todo_2")
	assert.Contains(t, output.Reminder, "todo_3")
}

// requireMeta asserts that result.Meta is a []Todo of the expected length.
func requireMeta(t *testing.T, result *tools.ToolCallResult, expectedLen int) {
	t.Helper()
	metaTodos, ok := result.Meta.([]Todo)
	require.True(t, ok, "Meta should be []Todo")
	require.Len(t, metaTodos, expectedLen)
}

func TestCreator_SharedScopes(t *testing.T) {
	t.Parallel()

	build := func(ctx context.Context, shared latest.SharedScope) tools.ToolSet {
		ts, err := Creator(ctx, latest.Toolset{Type: "todo", Shared: shared}, "", nil, "")
		require.NoError(t, err)
		return ts
	}

	t.Run("none is private per agent", func(t *testing.T) {
		t.Parallel()
		ctx := tools.WithTeamScope(t.Context())
		a, b := build(ctx, latest.SharedScopeNone), build(ctx, latest.SharedScopeNone)
		assert.NotSame(t, a, b)
	})

	t.Run("process is shared across teams", func(t *testing.T) {
		t.Parallel()
		a := build(tools.WithTeamScope(t.Context()), latest.SharedScopeProcess)
		b := build(tools.WithTeamScope(t.Context()), latest.SharedScopeProcess)
		assert.Same(t, a, b)
	})

	t.Run("team is shared within a team only", func(t *testing.T) {
		t.Parallel()
		teamA := tools.WithTeamScope(t.Context())
		teamB := tools.WithTeamScope(t.Context())

		first, second := build(teamA, latest.SharedScopeTeam), build(teamA, latest.SharedScopeTeam)
		assert.Same(t, first, second)
		assert.NotSame(t, first, build(teamB, latest.SharedScopeTeam))
		assert.NotSame(t, first, build(teamA, latest.SharedScopeProcess))
	})

	t.Run("team without a team scope is private", func(t *testing.T) {
		t.Parallel()
		a, b := build(t.Context(), latest.SharedScopeTeam), build(t.Context(), latest.SharedScopeTeam)
		assert.NotSame(t, a, b)
	})
}
