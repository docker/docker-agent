package path

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePathInDirectory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.yaml")
	err := os.WriteFile(testFile, []byte("test: value"), 0o644)
	require.NoError(t, err)

	tests := []struct {
		name        string
		path        string
		allowedDir  string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid path within directory",
			path:        filepath.Join(tmpDir, "test.yaml"),
			allowedDir:  tmpDir,
			expectError: false,
		},
		{
			name:        "valid relative path",
			path:        "test.yaml",
			allowedDir:  tmpDir,
			expectError: false,
		},
		{
			name:        "path traversal attempt with ../",
			path:        "../../../etc/passwd",
			allowedDir:  tmpDir,
			expectError: true,
			errorMsg:    "path outside allowed directory",
		},
		{
			name:        "path traversal attempt with subdirectory",
			path:        filepath.Join(tmpDir, "..", "..", "..", "etc", "passwd"),
			allowedDir:  tmpDir,
			expectError: true,
			errorMsg:    "path outside allowed directory",
		},
		{
			name:        "path with .. in middle",
			path:        "subdir/../test.yaml",
			allowedDir:  tmpDir,
			expectError: false,
		},
		{
			name:        "file name starting with two dots",
			path:        "..env",
			allowedDir:  tmpDir,
			expectError: false,
		},
		{
			name:        "directory name starting with two dots",
			path:        filepath.Join("..config", "agent.yaml"),
			allowedDir:  tmpDir,
			expectError: false,
		},
		{
			name:        "parent directory itself",
			path:        "..",
			allowedDir:  tmpDir,
			expectError: true,
			errorMsg:    "path outside allowed directory",
		},
		{
			name:        "sibling directory sharing the prefix",
			path:        filepath.Join("..", filepath.Base(tmpDir)+"-other", "test.yaml"),
			allowedDir:  tmpDir,
			expectError: true,
			errorMsg:    "path outside allowed directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ValidatePathInDirectory(tt.path, tt.allowedDir)

			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errorMsg)
				require.Empty(t, result)
			} else {
				require.NoError(t, err)
				require.NotEmpty(t, result)
				absAllowedDir, _ := filepath.Abs(tt.allowedDir)
				require.True(t, IsWithin(result, absAllowedDir))
			}
		})
	}
}

func TestValidatePathInDirectoryWithoutAllowedDir(t *testing.T) {
	t.Parallel()

	// A volume-qualified root, so the absolute cases are absolute on Windows
	// too (`\opt` alone has no volume there and is treated as relative).
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)

	tests := []struct {
		name        string
		path        string
		expectError bool
	}{
		{name: "relative path", path: "agent.yaml"},
		{name: "relative name starting with two dots", path: "..env"},
		{name: "relative directory starting with two dots", path: filepath.Join("..config", "agent.yaml")},
		{name: "relative parent traversal", path: filepath.Join("..", "agent.yaml"), expectError: true},
		{name: "relative parent directory", path: "..", expectError: true},
		{name: "absolute path", path: filepath.Join(root, "opt", "agent.yaml")},
		{name: "absolute name containing two dots", path: filepath.Join(root, "opt", "app..v2", "agent.yaml")},
		{name: "absolute parent traversal", path: root + strings.Join([]string{"opt", "..", "etc", "passwd"}, string(filepath.Separator)), expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ValidatePathInDirectory(tt.path, "")

			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), "directory traversal")
				require.Empty(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, filepath.Clean(tt.path), result)
			}
		})
	}
}
