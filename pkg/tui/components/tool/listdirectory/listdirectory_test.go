package listdirectory

import (
	"encoding/json"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
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

func TestExtractResultRedactedMetadata(t *testing.T) {
	t.Parallel()
	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	registry := hooks.NewRegistry()
	require.NoError(t, builtins.Register(registry))
	executor := hooks.NewExecutorWithRegistry(&hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.RedactSecrets}}}}}, t.TempDir(), nil, registry)
	result, err := executor.Dispatch(t.Context(), hooks.EventToolResponseTransform, &hooks.Input{ToolResponse: "listed", ToolResultMeta: filesystem.ListDirectoryMeta{Files: []string{secret}, Dirs: []string{"dir"}, Truncated: true}})
	require.NoError(t, err)
	require.NotNil(t, result.UpdatedToolResultMeta)
	assert.NotContains(t, string(result.UpdatedToolResultMeta), secret)
	assert.Contains(t, string(result.UpdatedToolResultMeta), portcullis.Marker)
	var decoded any
	require.NoError(t, json.Unmarshal(result.UpdatedToolResultMeta, &decoded))
	for _, meta := range []any{result.UpdatedToolResultMeta, decoded} {
		assert.Equal(t, "1 file and 1 directory (truncated)", extractResult(&types.Message{ToolResult: &tools.ToolCallResult{Meta: meta}}))
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
