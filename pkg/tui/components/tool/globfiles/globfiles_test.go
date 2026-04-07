package globfiles

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestExtractResult(t *testing.T) {
	tests := []struct {
		name     string
		meta     *builtin.GlobFilesMeta
		expected string
	}{
		{
			name:     "nil meta",
			meta:     nil,
			expected: "no matches",
		},
		{
			name:     "zero files",
			meta:     &builtin.GlobFilesMeta{FileCount: 0},
			expected: "no matches",
		},
		{
			name:     "single file",
			meta:     &builtin.GlobFilesMeta{FileCount: 1},
			expected: "1 file",
		},
		{
			name:     "multiple files",
			meta:     &builtin.GlobFilesMeta{FileCount: 42},
			expected: "42 files",
		},
		{
			name:     "truncated results",
			meta:     &builtin.GlobFilesMeta{FileCount: 100, Truncated: true},
			expected: "100 files (truncated)",
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
			name:     "simple pattern",
			args:     `{"pattern": "**/*.go"}`,
			expected: "**/*.go",
		},
		{
			name:     "pattern with path",
			args:     `{"pattern": "*.ts", "path": "src/components"}`,
			expected: "*.ts in src/components",
		},
		{
			name:     "pattern with dot path",
			args:     `{"pattern": "**/*.json", "path": "."}`,
			expected: "**/*.json",
		},
		{
			name:     "long pattern gets truncated",
			args:     `{"pattern": "src/very/deeply/nested/directory/structure/**/*.test.ts"}`,
			expected: "src/very/deeply/nested/directory/stru...",
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
