package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesystemtypes "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestEditFileRecordsAggregateDiff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, before, after string
		edits               []Edit
	}{
		{"deletion", "package main\n\nimport (\n\t\"context\"\n\t\"fmt\"\n)\n", "package main\n\nimport (\n\t\"fmt\"\n)\n", []Edit{{OldText: "\t\"context\"\n"}}},
		{"sequential", "first\nsecond\nthird\n", "first\nthird\n", []Edit{{OldText: "second\n", NewText: "temporary\n"}, {OldText: "temporary\n"}}},
		{"repeated text", "same\nsame\n", "same\n", []Edit{{OldText: "same\n"}}},
		{"whole file", "obsolete\n", "", []Edit{{OldText: "obsolete\n"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			require.NoError(t, os.WriteFile(path, []byte(tt.before), 0o644))
			result, err := New(dir).handleEditFile(t.Context(), EditFileArgs{Path: "file.txt", Edits: tt.edits})
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
			meta, ok := filesystemtypes.DecodeEditFileMeta(result.Meta)
			require.True(t, ok)
			expected, err := filesystemtypes.NewEditFileMeta("file.txt", tt.before, tt.after)
			require.NoError(t, err)
			assert.Equal(t, expected, meta)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.after, string(after))
		})
	}
}
