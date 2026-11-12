//go:build !js

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchPostEdit(t *testing.T) {
	ctx := t.Context()
	workDir := filepath.Join(string(filepath.Separator), "workspace", "app")

	tests := []struct {
		name       string
		pattern    string
		workingDir string
		filePath   string
		wantMatch  bool
	}{
		{
			name:       "basename pattern matches simple file",
			pattern:    "*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "main.go"),
			wantMatch:  true,
		},
		{
			name:       "basename pattern matches nested file",
			pattern:    "*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "sub", "foo.go"),
			wantMatch:  true,
		},
		{
			name:       "path-scoped pattern matches relative subpath",
			pattern:    "pkg/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "foo.go"),
			wantMatch:  true,
		},
		{
			name:       "regression test: pkg/*.go does not match pkg/sub/file.go",
			pattern:    "pkg/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "sub", "file.go"),
			wantMatch:  false,
		},
		{
			name:       "path-scoped pattern does not match different subpath",
			pattern:    "cmd/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "foo.go"),
			wantMatch:  false,
		},
		{
			name:       "nested slash pattern matches multi-level path",
			pattern:    "pkg/sub/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "sub", "bar.go"),
			wantMatch:  true,
		},
		{
			name:       "empty working dir falls back to slash-normalized file path",
			pattern:    "*.go",
			workingDir: "",
			filePath:   filepath.Join("pkg", "foo.go"),
			wantMatch:  true,
		},
		{
			name:       "invalid pattern returns false",
			pattern:    "[invalid",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "foo.go"),
			wantMatch:  false,
		},
		{
			name:       "file outside workingDir does not match path-scoped pattern",
			pattern:    "pkg/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "..", "outside", "foo.go"),
			wantMatch:  false,
		},
		{
			name:       "relative workingDir vs absolute filePath debug log fallback does not match relative pattern",
			pattern:    "pkg/*.go",
			workingDir: "relative/dir",
			filePath:   filepath.Join(workDir, "pkg", "foo.go"),
			wantMatch:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var relPath string
			if tt.workingDir != "" {
				rel, err := filepath.Rel(tt.workingDir, tt.filePath)
				if err == nil {
					relPath = filepath.ToSlash(rel)
				} else {
					relPath = filepath.ToSlash(tt.filePath)
				}
			} else {
				relPath = filepath.ToSlash(tt.filePath)
			}
			got := matchPostEdit(ctx, tt.pattern, relPath, tt.filePath)
			assert.Equal(t, tt.wantMatch, got)
		})
	}
}

func TestPostEditCommandsUseWorkingDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	workspace := t.TempDir()
	filePath := filepath.Join(workspace, "edited file.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("updated"), 0o600))

	err := runPostEditCommands(t.Context(), workspace, []PostEditConfig{
		{Path: "*.txt", Cmd: `cat "${file}" > hook-output.txt`},
	}, filePath)
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(workspace, "hook-output.txt"))
	require.NoError(t, err)
	assert.Equal(t, "updated", string(content))
}

func TestPostEditCommandsHonorCancellation(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runPostEditCommands(ctx, workspace, []PostEditConfig{
		{Path: "*.txt", Cmd: "printf unexpected > hook-output"},
	}, filepath.Join(workspace, "file.txt"))
	require.ErrorIs(t, err, context.Canceled)
	assert.NoFileExists(t, filepath.Join(workspace, "hook-output"))
}

func TestPostEditCommandsWithRelativeWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	t.Chdir(t.TempDir())
	require.NoError(t, os.Mkdir("workspace", 0o700))
	filePath := filepath.Join("workspace", "file.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("updated"), 0o600))
	err := runPostEditCommands(t.Context(), "workspace", []PostEditConfig{
		{Path: "*.txt", Cmd: `cat "${file}" > hook-output.txt`},
	}, filePath)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join("workspace", "hook-output.txt"))
	require.NoError(t, err)
	assert.Equal(t, "updated", string(data))
}
