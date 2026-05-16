package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func newTestTasksTool(t *testing.T) *Tool {
	t.Helper()
	dir := t.TempDir()
	return NewTasksTool(filepath.Join(dir, "tasks.json"))
}

func TestTasksTool_DisplayNames(t *testing.T) {
	tool := newTestTasksTool(t)

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tl := range all {
		assert.NotEmpty(t, tl.DisplayName())
		assert.NotEqual(t, tl.Name, tl.DisplayName())
	}
}

func TestTasksTool_CreateTask(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.createTask(t.Context(), CreateTaskArgs{
		Title:       "Build feature",
		Description: "Implement the new feature",
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var task Task
	require.NoError(t, json.Unmarshal([]byte(result.Output), &task))

	assert.NotEmpty(t, task.ID)
	assert.Equal(t, "Build feature", task.Title)
	assert.Equal(t, "Implement the new feature", task.Description)
	assert.Equal(t, PriorityMedium, task.Priority)
	assert.Equal(t, StatusPending, task.Status)
	assert.Empty(t, task.Dependencies)
	assert.NotEmpty(t, task.CreatedAt)
	assert.NotEmpty(t, task.UpdatedAt)
}

func TestTasksTool_CreateTask_WithPriority(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.createTask(t.Context(), CreateTaskArgs{
		Title:    "Critical bug",
		Priority: "critical",
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var task Task
	require.NoError(t, json.Unmarshal([]byte(result.Output), &task))
	assert.Equal(t, PriorityCritical, task.Priority)
}

func TestTasksTool_CreateTask_InvalidPriority(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.createTask(t.Context(), CreateTaskArgs{
		Title:    "Bad priority",
		Priority: "urgent",
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "invalid priority")
}

func TestTasksTool_CreateTask_WithDependencies(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, err := tool.createTask(t.Context(), CreateTaskArgs{Title: "Task A"})
	require.NoError(t, err)
	var taskA Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &taskA))

	r2, err := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "Task B",
		Dependencies: []string{taskA.ID},
	})
	require.NoError(t, err)
	assert.False(t, r2.IsError)

	var taskB Task
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &taskB))
	assert.Equal(t, []string{taskA.ID}, taskB.Dependencies)
}

func TestTasksTool_CreateTask_InvalidDependency(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "Task with bad dep",
		Dependencies: []string{"nonexistent"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "dependency task not found")
}

func TestTasksTool_CreateTask_FromFile(t *testing.T) {
	tool := newTestTasksTool(t)

	mdFile := filepath.Join(tool.basePath, "desc.md")
	require.NoError(t, os.WriteFile(mdFile, []byte("# Description\nFrom file"), 0o644))

	result, err := tool.createTask(t.Context(), CreateTaskArgs{
		Title: "File task",
		Path:  mdFile,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var task Task
	require.NoError(t, json.Unmarshal([]byte(result.Output), &task))
	assert.Equal(t, "# Description\nFrom file", task.Description)
}

func TestTasksTool_GetTask(t *testing.T) {
	tool := newTestTasksTool(t)

	r, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Test"})
	var created Task
	require.NoError(t, json.Unmarshal([]byte(r.Output), &created))

	result, err := tool.getTask(t.Context(), GetTaskArgs{ID: created.ID})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var got taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &got))
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, StatusPending, got.EffectiveStatus)
}

func TestTasksTool_GetTask_Blocked(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Blocker"})
	var blocker Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &blocker))

	r2, _ := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "Blocked",
		Dependencies: []string{blocker.ID},
	})
	var blocked Task
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &blocked))

	result, err := tool.getTask(t.Context(), GetTaskArgs{ID: blocked.ID})
	require.NoError(t, err)

	var got taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &got))
	assert.Equal(t, StatusBlocked, got.EffectiveStatus)
}

func TestTasksTool_GetTask_NotFound(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.getTask(t.Context(), GetTaskArgs{ID: "nonexistent"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestTasksTool_UpdateTask(t *testing.T) {
	tool := newTestTasksTool(t)

	r, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Original"})
	var task Task
	require.NoError(t, json.Unmarshal([]byte(r.Output), &task))

	result, err := tool.updateTask(t.Context(), UpdateTaskArgs{
		ID:       task.ID,
		Title:    "Updated",
		Priority: "high",
		Status:   "in_progress",
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var updated Task
	require.NoError(t, json.Unmarshal([]byte(result.Output), &updated))
	assert.Equal(t, "Updated", updated.Title)
	assert.Equal(t, PriorityHigh, updated.Priority)
	assert.Equal(t, StatusInProgress, updated.Status)
}

func TestTasksTool_UpdateTask_NotFound(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.updateTask(t.Context(), UpdateTaskArgs{ID: "nope", Title: "X"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestTasksTool_UpdateTask_InvalidStatus(t *testing.T) {
	tool := newTestTasksTool(t)

	r, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "T"})
	var task Task
	require.NoError(t, json.Unmarshal([]byte(r.Output), &task))

	result, err := tool.updateTask(t.Context(), UpdateTaskArgs{ID: task.ID, Status: "invalid"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "invalid status")
}

func TestTasksTool_DeleteTask(t *testing.T) {
	tool := newTestTasksTool(t)

	r, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "To delete"})
	var task Task
	require.NoError(t, json.Unmarshal([]byte(r.Output), &task))

	result, err := tool.deleteTask(t.Context(), DeleteTaskArgs{ID: task.ID})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, task.ID)

	// Verify it's gone
	getResult, err := tool.getTask(t.Context(), GetTaskArgs{ID: task.ID})
	require.NoError(t, err)
	assert.True(t, getResult.IsError)
}

func TestTasksTool_DeleteTask_RemovesDependencies(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Dep"})
	var dep Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &dep))

	r2, _ := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "Dependent",
		Dependencies: []string{dep.ID},
	})
	var dependent Task
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &dependent))

	_, err := tool.deleteTask(t.Context(), DeleteTaskArgs{ID: dep.ID})
	require.NoError(t, err)

	getResult, err := tool.getTask(t.Context(), GetTaskArgs{ID: dependent.ID})
	require.NoError(t, err)

	var got taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(getResult.Output), &got))
	assert.Empty(t, got.Dependencies)
	assert.Equal(t, StatusPending, got.EffectiveStatus)
}

func TestTasksTool_DeleteTask_NotFound(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.deleteTask(t.Context(), DeleteTaskArgs{ID: "nope"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestTasksTool_ListTasks(t *testing.T) {
	tool := newTestTasksTool(t)

	tool.createTask(t.Context(), CreateTaskArgs{Title: "Low", Priority: "low"})           //nolint:errcheck // test setup
	tool.createTask(t.Context(), CreateTaskArgs{Title: "Critical", Priority: "critical"}) //nolint:errcheck // test setup
	tool.createTask(t.Context(), CreateTaskArgs{Title: "High", Priority: "high"})         //nolint:errcheck // test setup

	result, err := tool.listTasks(t.Context(), ListTasksArgs{})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var tasks []taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &tasks))
	require.Len(t, tasks, 3)
	assert.Equal(t, "Critical", tasks[0].Title)
	assert.Equal(t, "High", tasks[1].Title)
	assert.Equal(t, "Low", tasks[2].Title)
}

func TestTasksTool_ListTasks_FilterByStatus(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Pending"})
	var task1 Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &task1))

	tool.createTask(t.Context(), CreateTaskArgs{Title: "Also pending"}) //nolint:errcheck // test setup

	tool.updateTask(t.Context(), UpdateTaskArgs{ID: task1.ID, Status: "done"}) //nolint:errcheck // test setup

	result, err := tool.listTasks(t.Context(), ListTasksArgs{Status: "pending"})
	require.NoError(t, err)

	var tasks []taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &tasks))
	require.Len(t, tasks, 1)
	assert.Equal(t, "Also pending", tasks[0].Title)
}

func TestTasksTool_ListTasks_FilterByPriority(t *testing.T) {
	tool := newTestTasksTool(t)

	tool.createTask(t.Context(), CreateTaskArgs{Title: "High", Priority: "high"})      //nolint:errcheck // test setup
	tool.createTask(t.Context(), CreateTaskArgs{Title: "Low", Priority: "low"})        //nolint:errcheck // test setup
	tool.createTask(t.Context(), CreateTaskArgs{Title: "Also high", Priority: "high"}) //nolint:errcheck // test setup

	result, err := tool.listTasks(t.Context(), ListTasksArgs{Priority: "high"})
	require.NoError(t, err)

	var tasks []taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &tasks))
	require.Len(t, tasks, 2)
	for _, task := range tasks {
		assert.Equal(t, PriorityHigh, task.Priority)
	}
}

func TestTasksTool_ListTasks_BlockedLast(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Blocker", Priority: "low"})
	var blocker Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &blocker))

	tool.createTask(t.Context(), CreateTaskArgs{ //nolint:errcheck // test setup
		Title:        "Blocked critical",
		Priority:     "critical",
		Dependencies: []string{blocker.ID},
	})
	tool.createTask(t.Context(), CreateTaskArgs{Title: "Free medium"}) //nolint:errcheck // test setup

	result, err := tool.listTasks(t.Context(), ListTasksArgs{})
	require.NoError(t, err)

	var tasks []taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &tasks))
	require.Len(t, tasks, 3)
	// Blocked task should be last regardless of priority
	assert.Equal(t, StatusBlocked, tasks[2].EffectiveStatus)
	assert.Equal(t, "Blocked critical", tasks[2].Title)
}

func TestTasksTool_NextTask(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Blocker", Priority: "high"})
	var blocker Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &blocker))

	tool.createTask(t.Context(), CreateTaskArgs{ //nolint:errcheck // test setup
		Title:        "Blocked",
		Priority:     "critical",
		Dependencies: []string{blocker.ID},
	})
	tool.createTask(t.Context(), CreateTaskArgs{Title: "Free low", Priority: "low"}) //nolint:errcheck // test setup

	result, err := tool.nextTask(t.Context(), tools.ToolCall{})
	require.NoError(t, err)

	var task taskWithEffective
	require.NoError(t, json.Unmarshal([]byte(result.Output), &task))
	assert.Equal(t, "Blocker", task.Title)
}

func TestTasksTool_NextTask_NoneAvailable(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "Done"})
	var task Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &task))
	tool.updateTask(t.Context(), UpdateTaskArgs{ID: task.ID, Status: "done"}) //nolint:errcheck // test setup

	result, err := tool.nextTask(t.Context(), tools.ToolCall{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "No actionable tasks")
}

func TestTasksTool_AddDependency(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "A"})
	r2, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "B"})
	var taskA, taskB Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &taskA))
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &taskB))

	result, err := tool.addDependency(t.Context(), AddDependencyArgs{
		TaskID:      taskB.ID,
		DependsOnID: taskA.ID,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var updated Task
	require.NoError(t, json.Unmarshal([]byte(result.Output), &updated))
	assert.Contains(t, updated.Dependencies, taskA.ID)
}

func TestTasksTool_AddDependency_Cycle(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "A"})
	var taskA Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &taskA))

	r2, _ := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "B",
		Dependencies: []string{taskA.ID},
	})
	var taskB Task
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &taskB))

	result, err := tool.addDependency(t.Context(), AddDependencyArgs{
		TaskID:      taskA.ID,
		DependsOnID: taskB.ID,
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "cycle")
}

func TestTasksTool_AddDependency_Duplicate(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "A"})
	var taskA Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &taskA))

	r2, _ := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "B",
		Dependencies: []string{taskA.ID},
	})
	var taskB Task
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &taskB))

	result, err := tool.addDependency(t.Context(), AddDependencyArgs{
		TaskID:      taskB.ID,
		DependsOnID: taskA.ID,
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "dependency already exists")
}

func TestTasksTool_RemoveDependency(t *testing.T) {
	tool := newTestTasksTool(t)

	r1, _ := tool.createTask(t.Context(), CreateTaskArgs{Title: "A"})
	var taskA Task
	require.NoError(t, json.Unmarshal([]byte(r1.Output), &taskA))

	r2, _ := tool.createTask(t.Context(), CreateTaskArgs{
		Title:        "B",
		Dependencies: []string{taskA.ID},
	})
	var taskB Task
	require.NoError(t, json.Unmarshal([]byte(r2.Output), &taskB))

	result, err := tool.removeDependency(t.Context(), RemoveDependencyArgs{
		TaskID:      taskB.ID,
		DependsOnID: taskA.ID,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var updated Task
	require.NoError(t, json.Unmarshal([]byte(result.Output), &updated))
	assert.Empty(t, updated.Dependencies)
}

func TestTasksTool_RemoveDependency_NotFound(t *testing.T) {
	tool := newTestTasksTool(t)

	result, err := tool.removeDependency(t.Context(), RemoveDependencyArgs{
		TaskID:      "nope",
		DependsOnID: "other",
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestTasksTool_Persistence(t *testing.T) {
	dir := t.TempDir()
	storagePath := filepath.Join(dir, "tasks.json")

	tool1 := NewTasksTool(storagePath)
	r, err := tool1.createTask(t.Context(), CreateTaskArgs{Title: "Persistent"})
	require.NoError(t, err)
	var task Task
	require.NoError(t, json.Unmarshal([]byte(r.Output), &task))

	tool2 := NewTasksTool(storagePath)
	result, err := tool2.getTask(t.Context(), GetTaskArgs{ID: task.ID})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "Persistent")
}

func TestTasksTool_ParametersAreObjects(t *testing.T) {
	tool := newTestTasksTool(t)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tl := range allTools {
		if tl.Parameters == nil {
			continue
		}
		m, err := tools.SchemaToMap(tl.Parameters)
		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

func TestTasksTool_Instructions(t *testing.T) {
	tool := newTestTasksTool(t)
	assert.NotEmpty(t, tool.Instructions())
}
