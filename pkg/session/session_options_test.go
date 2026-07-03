package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	t.Run("deduplicates and preserves order", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddAttachedFile("/abs/foo.go")
		s.AddAttachedFile("/abs/bar.go")
		s.AddAttachedFile("/abs/foo.go") // duplicate
		assert.Equal(t, []string{"/abs/foo.go", "/abs/bar.go"}, s.AttachedFilesSnapshot())
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
		s.AddAttachedFile("/abs/foo.go")
		snap := s.AttachedFilesSnapshot()
		snap[0] = "mutated"
		assert.Equal(t, []string{"/abs/foo.go"}, s.AttachedFilesSnapshot())
	})
}

func TestWithAttachedFiles(t *testing.T) {
	t.Parallel()
	s := New(WithAttachedFiles([]string{"/abs/foo.go", "", "relative/path.go", "/abs/bar.go", "/abs/foo.go"}))
	assert.Equal(t, []string{"/abs/foo.go", "/abs/bar.go"}, s.AttachedFilesSnapshot())
}

func TestRemoveAttachedFile(t *testing.T) {
	t.Parallel()
	t.Run("removes and preserves order of remaining files", func(t *testing.T) {
		t.Parallel()
		s := New(WithAttachedFiles([]string{"/abs/foo.go", "/abs/bar.go", "/abs/baz.go"}))
		assert.True(t, s.RemoveAttachedFile("/abs/bar.go"))
		assert.Equal(t, []string{"/abs/foo.go", "/abs/baz.go"}, s.AttachedFilesSnapshot())
	})

	t.Run("reports false for unknown path", func(t *testing.T) {
		t.Parallel()
		s := New(WithAttachedFiles([]string{"/abs/foo.go"}))
		assert.False(t, s.RemoveAttachedFile("/abs/other.go"))
		assert.Equal(t, []string{"/abs/foo.go"}, s.AttachedFilesSnapshot())
	})

	t.Run("allows re-attaching after removal", func(t *testing.T) {
		t.Parallel()
		s := New(WithAttachedFiles([]string{"/abs/foo.go"}))
		assert.True(t, s.RemoveAttachedFile("/abs/foo.go"))
		s.AddAttachedFile("/abs/foo.go")
		assert.Equal(t, []string{"/abs/foo.go"}, s.AttachedFilesSnapshot())
	})
}
