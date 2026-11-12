package listdirectory

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestExtractResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		meta     *filesystem.ListDirectoryMeta
		expected string
	}{
		{
			name:     "nil meta",
			meta:     nil,
			expected: "empty directory",
		},
		{
			name:     "empty directory",
			meta:     &filesystem.ListDirectoryMeta{},
			expected: "empty directory",
		},
		{
			name:     "only files",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a", "b", "c"}},
			expected: "3 files",
		},
		{
			name:     "only one file",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a"}},
			expected: "1 file",
		},
		{
			name:     "only directories",
			meta:     &filesystem.ListDirectoryMeta{Dirs: []string{"a", "b"}},
			expected: "2 directories",
		},
		{
			name:     "only one directory",
			meta:     &filesystem.ListDirectoryMeta{Dirs: []string{"a"}},
			expected: "1 directory",
		},
		{
			name:     "mixed files and directories",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a", "b", "c"}, Dirs: []string{"d", "e"}},
			expected: "3 files and 2 directories",
		},
		{
			name:     "truncated output",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a", "b"}, Dirs: []string{"c"}, Truncated: true},
			expected: "2 files and 1 directory (truncated)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &types.Message{}
			if tt.meta != nil {
				msg.ToolResult = &tools.ToolCallResult{Meta: *tt.meta}
			}
			result := extractResult(msg)
			if result != tt.expected {
				t.Errorf("extractResult() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestExtractResult_NilOrForeignMeta(t *testing.T) {
	t.Parallel()
	for _, meta := range []any{nil, "unexpected"} {
		if got := extractResult(&types.Message{ToolResult: &tools.ToolCallResult{Meta: meta}}); got != "empty directory" {
			t.Errorf("extractResult() = %q, want %q", got, "empty directory")
		}
	}
}
