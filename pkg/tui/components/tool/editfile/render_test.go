package editfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// TestRenderEditFile_EndToEnd writes a temporary source file, builds an
// edit_file tool call against it, and renders both unified and split views.
// The test focuses on structural elements rather than exact escape sequences,
// which depend on the active theme.
func TestRenderEditFile_EndToEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")

	updated := `package main

import "fmt"

func main() {
	x := 10
	y := 20
	fmt.Println(x + y)
}
`
	// Simulate the post-execution state: file already contains the new content.
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))

	args := map[string]any{
		"path": path,
		"edits": []map[string]string{
			{
				"oldText": "\tx := 1\n\ty := 2",
				"newText": "\tx := 10\n\ty := 20",
			},
		},
	}
	encoded, err := json.Marshal(args)
	require.NoError(t, err)

	toolCall := tools.ToolCall{
		ID: "test-render-1",
		Function: tools.FunctionCall{
			Name:      "edit_file",
			Arguments: string(encoded),
		},
	}

	// Reset cache so the test is hermetic across runs.
	InvalidateCaches()
	t.Cleanup(InvalidateCaches)

	unified := renderEditFile(toolCall, 120, false, types.ToolStatusCompleted)
	split := renderEditFile(toolCall, 120, true, types.ToolStatusCompleted)

	for _, out := range []string{unified, split} {
		assert.NotEmpty(t, out)
		// Source content should appear in the diff regardless of theme escapes.
		assert.True(t, strings.Contains(out, "10") || strings.Contains(out, "20"))
	}

	added, removed := countDiffLines(toolCall, types.ToolStatusCompleted)
	assert.Equal(t, 2, added)
	assert.Equal(t, 2, removed)
}

func TestRenderEditFile_TabIndentedLineDoesNotPanic(t *testing.T) {
	t.Parallel()
	// Regression: tab-indented modified lines used to feed raw (1-byte-tab)
	// text into diffWords while chroma tokens were built from the
	// tab-expanded variant, producing out-of-bounds slice indices in
	// applyWordEmphasis. The fix routes both through prepareContent.
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")

	updated := "package main\n\nfunc main() {\n\tx := 10\n}\n"
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))

	args := map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"oldText": "\tx := 1", "newText": "\tx := 10"},
		},
	}
	encoded, _ := json.Marshal(args)
	toolCall := tools.ToolCall{
		ID:       "test-tab-1",
		Function: tools.FunctionCall{Name: "edit_file", Arguments: string(encoded)},
	}

	InvalidateCaches()
	t.Cleanup(InvalidateCaches)

	assert.NotPanics(t, func() {
		_ = renderEditFile(toolCall, 120, false, types.ToolStatusCompleted)
		_ = renderEditFile(toolCall, 120, true, types.ToolStatusCompleted)
	})
}

func TestEditFileViewFallsBackToToolHeaderWhenArgumentsCannotParse(t *testing.T) {
	t.Parallel()

	msg := types.ToolCallMessage("agent", tools.ToolCall{
		ID: "test-invalid-args",
		Function: tools.FunctionCall{
			Name:      "edit_file",
			Arguments: `{"path": "/tmp/file",`,
		},
	}, tools.Tool{Name: "edit_file"}, types.ToolStatusPending)

	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	_ = view.SetSize(80, 0)

	assert.Contains(t, ansi.Strip(view.View()), "edit_file")
}

func TestRenderEditFile_MissingFileReturnsEmptyDiff(t *testing.T) {
	t.Parallel()
	args := map[string]any{
		"path": "/nonexistent/path/that/does/not/exist.go",
		"edits": []map[string]string{
			{"oldText": "a", "newText": "b"},
		},
	}
	encoded, _ := json.Marshal(args)
	toolCall := tools.ToolCall{
		ID:       "test-missing-1",
		Function: tools.FunctionCall{Name: "edit_file", Arguments: string(encoded)},
	}

	InvalidateCaches()
	t.Cleanup(InvalidateCaches)

	// Should not panic on a missing source file.
	assert.NotPanics(t, func() {
		_ = renderEditFile(toolCall, 100, false, types.ToolStatusCompleted)
	})
}

// An edit whose oldText matches more than once is refused by the tool, but
// strings.Replace(..., 1) always produces a first-occurrence diff — so without
// the preview check the user approves a change that never happens, having been
// shown a diff that was never on offer.
func TestRenderEditFile_ConfirmationPreviewsRefusal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.py")

	const original = "def dev():\n    debug = True\n\ndef prod():\n    debug = True\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	render := func(t *testing.T, id string, edits []map[string]string, status types.ToolStatus) string {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{"path": path, "edits": edits})
		require.NoError(t, err)
		toolCall := tools.ToolCall{
			ID:       id,
			Function: tools.FunctionCall{Name: "edit_file", Arguments: string(encoded)},
		}
		InvalidateCaches()
		t.Cleanup(InvalidateCaches)
		return ansi.Strip(renderEditFile(toolCall, 120, false, status))
	}

	t.Run("ambiguous edit shows the refusal instead of a diff", func(t *testing.T) {
		out := render(t, "test-refusal-ambiguous",
			[]map[string]string{{"oldText": "    debug = True", "newText": "    debug = False"}},
			types.ToolStatusConfirmation)

		assert.Contains(t, out, "will be refused")
		assert.Contains(t, out, "appears 2 times")
		assert.NotContains(t, out, "debug = False",
			"a diff the tool will not apply must not be shown")
	})

	t.Run("empty oldText shows the refusal", func(t *testing.T) {
		out := render(t, "test-refusal-empty",
			[]map[string]string{{"oldText": "", "newText": "INJECTED"}},
			types.ToolStatusConfirmation)

		assert.Contains(t, out, "oldText must not be empty")
		assert.NotContains(t, out, "INJECTED")
	})

	// Control: the preview must not refuse what the tool would accept. The
	// first edit removes the duplicate that made the second one ambiguous, so
	// the reason has to be evaluated against the running content.
	t.Run("an earlier edit resolving a later ambiguity still previews", func(t *testing.T) {
		out := render(t, "test-refusal-chained",
			[]map[string]string{
				{"oldText": "def dev():\n    debug = True", "newText": "def dev():\n    debug = None"},
				{"oldText": "    debug = True", "newText": "    debug = False"},
			},
			types.ToolStatusConfirmation)

		assert.NotContains(t, out, "will be refused")
		assert.NotEmpty(t, out)
	})

	// Control: an unambiguous edit is unaffected.
	t.Run("a unique edit previews normally", func(t *testing.T) {
		out := render(t, "test-refusal-unique",
			[]map[string]string{{"oldText": "def prod():\n    debug = True", "newText": "def prod():\n    debug = False"}},
			types.ToolStatusConfirmation)

		assert.NotContains(t, out, "will be refused")
		assert.Contains(t, out, "debug = False")
	})

	// Control: after execution the file on disk is the outcome; there is
	// nothing left to predict and the diff must render as before.
	t.Run("a completed call is never previewed as refused", func(t *testing.T) {
		out := render(t, "test-refusal-completed",
			[]map[string]string{{"oldText": "    debug = True", "newText": "    debug = False"}},
			types.ToolStatusCompleted)

		assert.NotContains(t, out, "will be refused")
	})
}
