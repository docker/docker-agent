package dialog

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFilePickerDialog_ShowGitignored(t *testing.T) {
	// Create a temporary directory with a .gitignore file
	tmpDir := t.TempDir()

	// Initialize a git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	err := cmd.Run()
	require.NoError(t, err)

	// Create .gitignore
	gitignoreContent := "*.log\ntest.txt\n"
	err = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(gitignoreContent), 0o644)
	require.NoError(t, err)

	// Create files that should be ignored
	err = os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("log content"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("test content"), 0o644)
	require.NoError(t, err)

	// Create a file that should NOT be ignored
	err = os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("readme content"), 0o644)
	require.NoError(t, err)

	t.Run("without show gitignored flag", func(t *testing.T) {
		d := NewFilePickerDialog(tmpDir, false).(*filePickerDialog)

		// Count visible files (excluding .. and .git directory)
		visibleFiles := 0
		for _, entry := range d.entries {
			if entry.name != ".." && entry.name != ".git" {
				visibleFiles++
			}
		}

		// Should only show readme.md (not the gitignored files)
		assert.Equal(t, 1, visibleFiles, "Should only show non-gitignored files")

		// Verify readme.md is present
		foundReadme := false
		for _, entry := range d.entries {
			if entry.name == "readme.md" {
				foundReadme = true
				break
			}
		}
		assert.True(t, foundReadme, "Should include readme.md")
	})

	t.Run("with show gitignored flag", func(t *testing.T) {
		d := NewFilePickerDialog(tmpDir, true).(*filePickerDialog)

		// Count visible files (excluding .. and .git directory)
		visibleFiles := 0
		for _, entry := range d.entries {
			if entry.name != ".." && entry.name != ".git" {
				visibleFiles++
			}
		}

		// Should show all files including gitignored ones
		assert.Equal(t, 3, visibleFiles, "Should show all files when showGitignored is true")

		// Verify all files are present
		foundReadme := false
		foundLog := false
		foundTest := false
		for _, entry := range d.entries {
			switch entry.name {
			case "readme.md":
				foundReadme = true
			case "debug.log":
				foundLog = true
			case "test.txt":
				foundTest = true
			}
		}
		assert.True(t, foundReadme, "Should include readme.md")
		assert.True(t, foundLog, "Should include debug.log (gitignored)")
		assert.True(t, foundTest, "Should include test.txt (gitignored)")
	})
}
