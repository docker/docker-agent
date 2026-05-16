package lsp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// newTestMultiplexer creates a multiplexer with a Go and Python backend.
func newTestMultiplexer() (*Multiplexer, *Tool) {
	goTool := NewLSPTool("gopls", nil, nil, "/tmp")
	goTool.SetFileTypes([]string{".go", ".mod"})

	pyTool := NewLSPTool("pyright", nil, nil, "/tmp")
	pyTool.SetFileTypes([]string{".py"})

	mux := NewLSPMultiplexer([]Backend{
		{LSP: goTool, Toolset: goTool},
		{LSP: pyTool, Toolset: pyTool},
	})
	return mux, goTool
}

// findTool returns the tool with the given name, or fails the test.
func findTool(t *testing.T, allTools []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tool := range allTools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return tools.Tool{}
}

// callHover is a shorthand to invoke lsp_hover on a given file through the multiplexer.
func callHover(t *testing.T, mux *Multiplexer, args string) *tools.ToolCallResult {
	t.Helper()
	allTools, err := mux.Tools(t.Context())
	require.NoError(t, err)
	hover := findTool(t, allTools, ToolNameLSPHover)
	tc := tools.ToolCall{Function: tools.FunctionCall{Name: ToolNameLSPHover, Arguments: args}}
	result, err := hover.Handler(t.Context(), tc)
	require.NoError(t, err)
	return result
}

func TestLSPMultiplexer_Tools_NoDuplicates(t *testing.T) {
	t.Parallel()

	mux, goTool := newTestMultiplexer()

	allTools, err := mux.Tools(t.Context())
	require.NoError(t, err)

	// Should have the same number of tools as a single LSP backend.
	singleTools, err := goTool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, len(singleTools))

	// No duplicate tool names.
	seen := make(map[string]bool)
	for _, tool := range allTools {
		assert.False(t, seen[tool.Name], "duplicate tool name: %s", tool.Name)
		seen[tool.Name] = true
	}
}

func TestLSPMultiplexer_RoutesToCorrectBackend(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()

	// .go → routes to gopls, .py → routes to pyright.
	// Both backends are not running so they will auto-init and respond with
	// some output — we just verify routing produces a non-empty response.
	for _, file := range []string{"/tmp/main.go", "/tmp/app.py"} {
		result := callHover(t, mux, `{"file": "`+file+`", "line": 1, "character": 1}`)
		assert.NotEmpty(t, result.Output, "expected output for %s", file)
	}
}

func TestLSPMultiplexer_NoBackendForFile(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()
	result := callHover(t, mux, `{"file": "/tmp/main.rs", "line": 1, "character": 1}`)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "no LSP server configured for file")
}

func TestLSPMultiplexer_EmptyFileArgument(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()
	result := callHover(t, mux, `{"line": 1, "character": 1}`)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "file argument is required")
}

func TestLSPMultiplexer_InvalidJSON(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()
	result := callHover(t, mux, `{invalid`)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "failed to parse file argument")
}

func TestLSPMultiplexer_Instructions(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()
	instructions := mux.Instructions()
	assert.Contains(t, instructions, "lsp_hover")
	assert.Contains(t, instructions, "Stateless")

	// Both backends share the same instructions — "Stateless" should appear only once.
	assert.Equal(t, 1, strings.Count(instructions, "Stateless"),
		"identical instructions should be deduplicated")
}

func TestLSPMultiplexer_WorkspaceToolBroadcasts(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()

	allTools, err := mux.Tools(t.Context())
	require.NoError(t, err)
	workspace := findTool(t, allTools, ToolNameLSPWorkspace)

	args, _ := json.Marshal(WorkspaceArgs{})
	tc := tools.ToolCall{Function: tools.FunctionCall{Name: ToolNameLSPWorkspace, Arguments: string(args)}}
	result, err := workspace.Handler(t.Context(), tc)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Output)
}

func TestLSPMultiplexer_Stop_NotStarted(t *testing.T) {
	t.Parallel()

	mux, _ := newTestMultiplexer()
	require.NoError(t, mux.Stop(t.Context()))
}
