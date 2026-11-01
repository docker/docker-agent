package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func callWorkspaceTool(t *testing.T, ts tools.ToolSet, name string, args any) *tools.ToolCallResult {
	t.Helper()
	all, err := ts.Tools(t.Context())
	require.NoError(t, err)
	i := slices.IndexFunc(all, func(tool tools.Tool) bool { return tool.Name == name })
	require.NotEqual(t, -1, i)
	data, err := json.Marshal(args)
	require.NoError(t, err)
	result, err := all[i].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{Name: name, Arguments: string(data)},
	}, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func TestPlanCreatorSharesStorageAndNotifier(t *testing.T) {
	t.Parallel()
	shared, err := CreateToolSet()
	require.NoError(t, err)

	var workspaces []tools.ToolSet
	for range 2 {
		dir := t.TempDir()
		rc := &config.RuntimeConfig{Config: config.Config{WorkingDir: dir}}
		ts, err := Creator(t.Context(), latest.Toolset{Type: "plan"}, "", rc, "")
		require.NoError(t, err)
		workspace, ok := ts.(*workspaceToolSet)
		require.True(t, ok)
		assert.Equal(t, dir, workspace.workingDir)
		assert.Same(t, shared, workspace.shared)
		assert.Same(t, SharedStorage(), workspace.shared.storage)
		notifiers := tools.FindAll[ChangeNotifier](tools.NewStartable(ts))
		require.Len(t, notifiers, 1)
		assert.Same(t, shared, notifiers[0], "runtime subscriptions must deduplicate across workspaces")
		assert.Equal(t, tools.GetInstructions(shared), tools.GetInstructions(ts))
		rc.WorkingDir = t.TempDir()
		assert.Equal(t, dir, workspace.workingDir, "capture the workspace, not the mutable runtime config")
		workspaces = append(workspaces, ts)
	}
	assert.NotSame(t, workspaces[0], workspaces[1])
	assert.NotSame(t, shared, New())
	assert.NotSame(t, SharedStorage(), New().storage)
}

func TestPlanCreatorCapturesRelativeAndDefaultWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		rc   *config.RuntimeConfig
		want string
	}{
		{name: "nil config", want: cwd},
		{name: "empty working directory", rc: &config.RuntimeConfig{}, want: cwd},
		{
			name: "relative working directory",
			rc:   &config.RuntimeConfig{Config: config.Config{WorkingDir: "workspace"}},
			want: filepath.Join(cwd, "workspace"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := Creator(t.Context(), latest.Toolset{Type: "plan"}, "", tc.rc, "")
			require.NoError(t, err)
			workspace := ts.(*workspaceToolSet)
			assert.Equal(t, tc.want, workspace.workingDir)
			assert.Equal(t, filepath.Join(tc.want, "plan.md"), workspace.resolvePath("plan.md"))
		})
	}
}

func TestPlanWorkspaceImportExport(t *testing.T) {
	t.Parallel()
	shared := New(WithStorage(NewMemoryStorage()))
	a := &workspaceToolSet{shared: shared, workingDir: t.TempDir()}
	b := &workspaceToolSet{shared: shared, workingDir: t.TempDir()}

	result := callWorkspaceTool(t, a, ToolNameWritePlan, WritePlanArgs{Name: "p", Content: "original", Title: "Shared"})
	require.False(t, result.IsError, result.Output)

	relative := filepath.Join("notes", "plan.md")
	for _, workspace := range []*workspaceToolSet{a, b} {
		result := callWorkspaceTool(t, workspace, ToolNameExportPlanToFile, ExportPlanToFileArgs{Name: "p", Path: relative})
		require.False(t, result.IsError, result.Output)
		var exported ExportResult
		require.NoError(t, json.Unmarshal([]byte(result.Output), &exported))
		assert.Equal(t, filepath.Join(workspace.workingDir, relative), exported.Path)
		assert.Equal(t, 1, exported.Revision)
		assert.Equal(t, "Shared", exported.Title)
		assert.Equal(t, len("original"), exported.BytesWritten)
		content, err := os.ReadFile(exported.Path)
		require.NoError(t, err)
		assert.Equal(t, "original", string(content))
	}

	for i, workspace := range []*workspaceToolSet{a, b} {
		content := []string{"edited in a", "edited in b"}[i]
		require.NoError(t, os.WriteFile(filepath.Join(workspace.workingDir, relative), []byte(content), 0o600))
		result := callWorkspaceTool(t, workspace, ToolNameUpdatePlanFromFile, UpdatePlanFromFileArgs{
			Name: "p", Path: relative, LastKnownRevision: new(i + 1),
		})
		require.False(t, result.IsError, result.Output)
		for _, reader := range []*workspaceToolSet{a, b} {
			result := callWorkspaceTool(t, reader, ToolNameReadPlan, ReadPlanArgs{Name: "p"})
			require.False(t, result.IsError, result.Output)
			var plan Plan
			require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
			assert.Equal(t, content, plan.Content)
			assert.Equal(t, i+2, plan.Revision)
			assert.Equal(t, "Shared", plan.Title)
		}
	}
	content, err := os.ReadFile(filepath.Join(a.workingDir, relative))
	require.NoError(t, err)
	assert.Equal(t, "edited in a", string(content), "another workspace must not overwrite this workspace's export")

	result = callWorkspaceTool(t, a, ToolNameUpdatePlanFromFile, UpdatePlanFromFileArgs{Name: "p", Path: relative, LastKnownRevision: new(1)})
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "version conflict")
}

func TestPlanWorkspaceAbsolutePaths(t *testing.T) {
	t.Parallel()
	shared := New(WithStorage(NewMemoryStorage()))
	path := filepath.Join(t.TempDir(), "plan.md")
	for _, workspace := range []*workspaceToolSet{
		{shared: shared, workingDir: t.TempDir()},
		{shared: shared, workingDir: t.TempDir()},
	} {
		require.NoError(t, os.WriteFile(path, []byte("absolute content"), 0o600))
		result := callWorkspaceTool(t, workspace, ToolNameUpdatePlanFromFile, UpdatePlanFromFileArgs{Name: "p", Path: path})
		require.False(t, result.IsError, result.Output)
		require.NoError(t, os.Remove(path))
		result = callWorkspaceTool(t, workspace, ToolNameExportPlanToFile, ExportPlanToFileArgs{Name: "p", Path: path})
		require.False(t, result.IsError, result.Output)
		var exported ExportResult
		require.NoError(t, json.Unmarshal([]byte(result.Output), &exported))
		assert.Equal(t, path, exported.Path)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "absolute content", string(content))
	}
}

func TestPlanWorkspaceNotificationsAndErrors(t *testing.T) {
	t.Parallel()
	shared := New(WithStorage(NewMemoryStorage()))
	a := &workspaceToolSet{shared: shared, workingDir: t.TempDir()}
	b := &workspaceToolSet{shared: shared, workingDir: t.TempDir()}
	recA, recB := &changeRecorder{}, &changeRecorder{}
	notifierA, ok := tools.As[ChangeNotifier](a)
	require.True(t, ok)
	notifierB, ok := tools.As[ChangeNotifier](b)
	require.True(t, ok)
	unsubA := notifierA.SubscribeChanges(recA.callback())
	defer unsubA()
	defer notifierB.SubscribeChanges(recB.callback())()

	result := callWorkspaceTool(t, a, ToolNameWritePlan, WritePlanArgs{Name: "p", Content: "v1"})
	require.False(t, result.IsError, result.Output)
	result = callWorkspaceTool(t, b, ToolNameExportPlanToFile, ExportPlanToFileArgs{Name: "p", Path: "plan.md"})
	require.False(t, result.IsError, result.Output)
	result = callWorkspaceTool(t, b, ToolNameUpdatePlanFromFile, UpdatePlanFromFileArgs{Name: "p", Path: "plan.md"})
	require.False(t, result.IsError, result.Output)

	want := []Change{
		{Name: "p", Action: ChangeActionWrite, Revision: 1},
		{Name: "p", Action: ChangeActionWrite, Revision: 2},
	}
	assert.Equal(t, want, recA.changes)
	assert.Equal(t, want, recB.changes)

	for _, name := range []string{ToolNameExportPlanToFile, ToolNameUpdatePlanFromFile} {
		result := callWorkspaceTool(t, a, name, map[string]string{"name": "p", "path": ""})
		assert.True(t, result.IsError)
		assert.Equal(t, "path must not be empty", result.Output)
	}
	result = callWorkspaceTool(t, a, ToolNameUpdatePlanFromFile, UpdatePlanFromFileArgs{Name: "p", Path: "missing.md"})
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "does not exist")
	assert.Equal(t, want, recA.changes, "exports and failed imports must not notify")
	assert.Equal(t, want, recB.changes)

	unsubA()
	unsubA()
	result = callWorkspaceTool(t, b, ToolNameSetPlanStatus, SetPlanStatusArgs{Name: "p", Status: "done"})
	require.False(t, result.IsError, result.Output)
	result = callWorkspaceTool(t, a, ToolNameDeletePlan, DeletePlanArgs{Name: "p"})
	require.False(t, result.IsError, result.Output)
	assert.Equal(t, want, recA.changes)
	assert.Equal(t, append(want,
		Change{Name: "p", Action: ChangeActionStatus, Revision: 3},
		Change{Name: "p", Action: ChangeActionDelete},
	), recB.changes)
}
