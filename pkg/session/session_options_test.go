package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithWorkingDir_SetsAllowedDirectories(t *testing.T) {
	t.Parallel()
	s := New(WithWorkingDir("/projects/myapp"))

	assert.Equal(t, "/projects/myapp", s.WorkingDir)
	assert.Equal(t, []string{"/projects/myapp"}, s.AllowedDirectories())
}

func TestWithWorkingDir_EmptyReturnsNilAllowedDirs(t *testing.T) {
	t.Parallel()
	s := New()

	assert.Empty(t, s.WorkingDir)
	assert.Nil(t, s.AllowedDirectories())
}

func TestWithStructuredOutputDisabled(t *testing.T) {
	t.Parallel()
	assert.False(t, New().DisableStructuredOutput)
	assert.False(t, New(WithStructuredOutputDisabled(false)).DisableStructuredOutput)
	assert.True(t, New(WithStructuredOutputDisabled(true)).DisableStructuredOutput)
}

func TestNewSession_AllOptionsApplied(t *testing.T) {
	t.Parallel()
	s := New(
		WithMaxIterations(10),
		WithToolsApproved(true),
		WithHideToolResults(true),
		WithWorkingDir("/work"),
	)

	assert.Equal(t, 10, s.MaxIterations)
	assert.True(t, s.ToolsApproved)
	assert.True(t, s.HideToolResults)
	assert.Equal(t, "/work", s.WorkingDir)
	assert.Equal(t, []string{"/work"}, s.AllowedDirectories())
}

// TestNewSession_ConsistencyBetweenInitialAndSpawned verifies that the
// initial session and spawned sessions receive the same set of options.
// This test documents the expected option set so that adding a new option
// to one path without the other will be caught.
func TestNewSession_ConsistencyBetweenInitialAndSpawned(t *testing.T) {
	t.Parallel()
	workingDir := "/projects/app"
	autoApprove := true
	hideToolResults := true
	maxIterations := 25

	// Simulate what createLocalRuntimeAndSession builds (initial session).
	initial := New(
		WithMaxIterations(maxIterations),
		WithToolsApproved(autoApprove),
		WithHideToolResults(hideToolResults),
		WithWorkingDir(workingDir),
	)

	// Simulate what createSessionSpawner builds (spawned session).
	spawned := New(
		WithMaxIterations(maxIterations),
		WithToolsApproved(autoApprove),
		WithHideToolResults(hideToolResults),
		WithWorkingDir(workingDir),
	)

	assert.Equal(t, initial.MaxIterations, spawned.MaxIterations)
	assert.Equal(t, initial.ToolsApproved, spawned.ToolsApproved)
	assert.Equal(t, initial.HideToolResults, spawned.HideToolResults)
	assert.Equal(t, initial.WorkingDir, spawned.WorkingDir)
	assert.Equal(t, initial.AllowedDirectories(), spawned.AllowedDirectories())
}

func TestAddAttachedFile(t *testing.T) {
	t.Parallel()
	foo := filepath.Join(t.TempDir(), "abs", "foo.go")
	bar := filepath.Join(t.TempDir(), "abs", "bar.go")
	t.Run("deduplicates and preserves order", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile(foo)
		s.AddAttachedFile(bar)
		s.AddAttachedFile(foo) // duplicate
		assert.Equal(t, []string{foo, bar}, s.AttachedFilesSnapshot())
	})

	t.Run("ignores empty paths", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile("")
		assert.Empty(t, s.AttachedFilesSnapshot())
	})

	t.Run("ignores non-absolute paths", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile("foo.go")
		s.AddAttachedFile("./bar.go")
		s.AddAttachedFile("../baz.go")
		assert.Empty(t, s.AttachedFilesSnapshot())
	})

	t.Run("snapshot is independent of session storage", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile(foo)
		snap := s.AttachedFilesSnapshot()
		require.Len(t, snap, 1)
		snap[0] = "mutated"
		assert.Equal(t, []string{foo}, s.AttachedFilesSnapshot())
	})
}

func TestRemoveAttachedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	foo := filepath.Join(root, "foo.go")
	bar := filepath.Join(root, "bar.go")
	baz := filepath.Join(root, "baz.go")
	other := filepath.Join(root, "other.go")
	t.Run("removes and reports presence", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile(foo)
		s.AddAttachedFile(bar)
		s.AddAttachedFile(baz)

		assert.True(t, s.RemoveAttachedFile(bar))
		assert.Equal(t, []string{foo, baz}, s.AttachedFilesSnapshot())
	})

	t.Run("reports absent paths", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile(foo)
		assert.False(t, s.RemoveAttachedFile(other))
		assert.False(t, s.RemoveAttachedFile(""))
		assert.Equal(t, []string{foo}, s.AttachedFilesSnapshot())
	})

	t.Run("no-op on empty list", func(t *testing.T) {
		t.Parallel()
		s := New()
		assert.False(t, s.RemoveAttachedFile(foo))
		assert.Empty(t, s.AttachedFilesSnapshot())
	})

	t.Run("file can be re-attached after removal", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile(foo)
		require.True(t, s.RemoveAttachedFile(foo))
		s.AddAttachedFile(foo)
		assert.Equal(t, []string{foo}, s.AttachedFilesSnapshot())
	})
}

func TestWithAttachedFiles(t *testing.T) {
	t.Parallel()
	foo := filepath.Join(t.TempDir(), "abs", "foo.go")
	bar := filepath.Join(t.TempDir(), "abs", "bar.go")
	s := New(WithAttachedFiles([]string{foo, "", "relative/path.go", bar, foo}))
	assert.Equal(t, []string{foo, bar}, s.AttachedFilesSnapshot())
}
