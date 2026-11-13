//go:build !js

package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesystemtypes "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestEditFileMetaIncludesPostEditChanges(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("commands use POSIX shell syntax")
	}
	for _, tt := range []struct {
		name, command, after string
		failed               bool
	}{
		{"formatter", `printf 'formatted\n' > "$file"`, "formatted\n", false},
		{"failed formatter", `printf 'partial\n' > "$file"; exit 1`, "partial\n", true},
		{"removed file", `rm "$file"`, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			require.NoError(t, os.WriteFile(path, []byte("original\n"), 0o644))
			tool := New(dir, WithPostEditCommands([]PostEditConfig{{Path: "*.txt", Cmd: tt.command}}))
			result, err := tool.handleEditFile(t.Context(), EditFileArgs{Path: "file.txt", Edits: []Edit{{OldText: "original", NewText: "edited"}}})
			require.NoError(t, err)
			assert.Equal(t, tt.failed, result.IsError)
			meta, ok := filesystemtypes.DecodeEditFileMeta(result.Meta)
			require.True(t, ok)
			expected, err := filesystemtypes.NewEditFileMeta("file.txt", "original\n", tt.after)
			require.NoError(t, err)
			assert.Equal(t, expected, meta)
		})
	}
}
