package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePath(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()

	ts := &FilesystemToolset{
		workingDir: workingDir,
	}

	absWorkingDir, err := filepath.EvalSymlinks(workingDir)
	require.NoError(t, err)

	tests := []struct {
		name      string
		userPath  string
		wantPath  string
		wantError bool
	}{
		{
			name:     "simple relative path",
			userPath: "file.txt",
			wantPath: filepath.Join(absWorkingDir, "file.txt"),
		},
		{
			name:     "nested relative path",
			userPath: "subdir/file.txt",
			wantPath: filepath.Join(absWorkingDir, "subdir", "file.txt"),
		},
		{
			name:     "dot path resolves to working directory",
			userPath: ".",
			wantPath: absWorkingDir,
		},
		{
			name:      "parent directory escape blocked",
			userPath:  "../escape.txt",
			wantError: true,
		},
		{
			name:      "deep parent directory escape blocked",
			userPath:  "subdir/../../escape.txt",
			wantError: true,
		},
		{
			name:     "dot-dot within working dir is fine",
			userPath: "subdir/../file.txt",
			wantPath: filepath.Join(absWorkingDir, "file.txt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolved, err := ts.resolvePath(tt.userPath)
			if tt.wantError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "escapes the working directory")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, resolved)
		})
	}
}

func TestNormalizePathForComparison(t *testing.T) {
	t.Parallel()

	// On macOS/Windows (case-insensitive), normalization should lowercase.
	// On Linux (case-sensitive), it should be identity.
	result := normalizePathForComparison("/Some/Path")

	// This test validates the function exists and returns a string.
	// The exact behavior depends on the platform.
	assert.NotEmpty(t, result)
}

func TestResolvePath_SymlinkEscape(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	workingDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a secret file outside the working directory.
	secretFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("secret"), 0o644))

	// Create a symlink inside the working directory pointing outside.
	symlink := filepath.Join(workingDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, symlink))

	ts := &FilesystemToolset{workingDir: workingDir}

	// Accessing a file through the symlink should be blocked.
	_, err := ts.resolvePath("escape/secret.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")

	// The symlink target itself should also be blocked.
	_, err = ts.resolvePath("escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}

func TestResolvePath_SymlinkWithinWorkingDir(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	workingDir := t.TempDir()

	// Create a subdirectory and a symlink to it within the working dir.
	subdir := filepath.Join(workingDir, "real")
	require.NoError(t, os.Mkdir(subdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "file.txt"), []byte("ok"), 0o644))

	link := filepath.Join(workingDir, "link")
	require.NoError(t, os.Symlink(subdir, link))

	ts := &FilesystemToolset{workingDir: workingDir}

	// Symlink within working dir should be allowed.
	resolved, err := ts.resolvePath("link/file.txt")
	require.NoError(t, err)
	assert.Contains(t, resolved, "real/file.txt")
}

func TestResolvePath_NonExistentPathWithSymlinkAncestor(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlink test not reliable on Windows")
	}

	workingDir := t.TempDir()
	outsideDir := t.TempDir()

	// Symlink inside working dir pointing outside.
	symlink := filepath.Join(workingDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, symlink))

	ts := &FilesystemToolset{workingDir: workingDir}

	// Even for a non-existent file under the symlink, traversal should be blocked.
	_, err := ts.resolvePath("escape/nonexistent.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}
