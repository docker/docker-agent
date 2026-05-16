package searchfilescontent

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestExtractResult(t *testing.T) {
	tests := []struct {
		name     string
		meta     *filesystem.SearchFilesContentMeta
		expected string
	}{
		{
			name:     "nil meta",
			meta:     nil,
			expected: "no matches",
		},
		{
			name:     "zero matches",
			meta:     &filesystem.SearchFilesContentMeta{MatchCount: 0, FileCount: 0},
			expected: "no matches",
		},
		{
			name:     "single match in single file",
			meta:     &filesystem.SearchFilesContentMeta{MatchCount: 1, FileCount: 1},
			expected: "1 match in 1 file",
		},
		{
			name:     "multiple matches in single file",
			meta:     &filesystem.SearchFilesContentMeta{MatchCount: 5, FileCount: 1},
			expected: "5 matches in 1 file",
		},
		{
			name:     "single match in multiple files",
			meta:     &filesystem.SearchFilesContentMeta{MatchCount: 1, FileCount: 3},
			expected: "1 match in 3 files",
		},
		{
			name:     "multiple matches in multiple files",
			meta:     &filesystem.SearchFilesContentMeta{MatchCount: 42, FileCount: 7},
			expected: "42 matches in 7 files",
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

func TestExtractArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "simple path and query",
			args:     `{"path": "src", "query": "TODO"}`,
			expected: "src (TODO)",
		},
		{
			name:     "regex query",
			args:     `{"path": ".", "query": "func.*Test", "is_regex": true}`,
			expected: ". (regex: func.*Test)",
		},
		{
			name:     "long query gets truncated",
			args:     `{"path": "pkg", "query": "this is a very long search query that should be truncated"}`,
			expected: "pkg (this is a very long search ...)",
		},
		{
			name:     "invalid json",
			args:     `invalid`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractArgs(tt.args)
			if result != tt.expected {
				t.Errorf("extractArgs(%q) = %q, want %q", tt.args, result, tt.expected)
			}
		})
	}
}
