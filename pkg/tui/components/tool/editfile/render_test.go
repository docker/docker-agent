package editfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aymanbagabas/go-udiff"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func editMessage(t *testing.T, path, before string, edits []filesystem.Edit) *types.Message {
	t.Helper()
	after, err := filesystem.ApplyEdits(before, edits)
	require.NoError(t, err)
	meta, err := filesystem.NewEditFileMeta(path, before, after)
	require.NoError(t, err)
	args, err := json.Marshal(filesystem.EditFileArgs{Path: path, Edits: edits})
	require.NoError(t, err)
	msg := types.ToolCallMessage("agent", tools.ToolCall{ID: t.Name(), Function: tools.FunctionCall{Name: "edit_file", Arguments: string(args)}}, tools.Tool{Name: "edit_file"}, types.ToolStatusCompleted)
	msg.ToolResult = &tools.ToolCallResult{Meta: meta}
	return msg
}

func TestRenderEditFileRecordedDiff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, before   string
		edits          []filesystem.Edit
		added, removed int
	}{
		{"deletion", "package main\n\nimport (\n\t\"context\"\n\t\"fmt\"\n)\n", []filesystem.Edit{{OldText: "\t\"context\"\n"}}, 0, 1},
		{"whole file", "obsolete\n", []filesystem.Edit{{OldText: "obsolete\n"}}, 0, 1},
		{"no trailing newline", "keep\nobsolete", []filesystem.Edit{{OldText: "obsolete"}}, 0, 1},
		{"tabs", "package main\n\nfunc main() {\n\tx := 1\n}\n", []filesystem.Edit{{OldText: "\tx := 1", NewText: "\tx := 10"}}, 1, 1},
		{"sequential edits", "first\nsecond\nthird\n", []filesystem.Edit{{OldText: "second\n", NewText: "intermediate\n"}, {OldText: "intermediate\n"}}, 0, 1},
		{"repeated replacement", "new\nold\n", []filesystem.Edit{{OldText: "old", NewText: "new"}}, 1, 1},
		{"no op", "same\n", []filesystem.Edit{{OldText: "same", NewText: "same"}}, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "main.go")
			msg := editMessage(t, path, tt.before, tt.edits)
			meta, ok := editFileDiff(msg)
			require.True(t, ok)
			// The recorded diff must survive a remote event or persisted session round trip.
			data, err := json.Marshal(msg.ToolResult)
			require.NoError(t, err)
			var decoded tools.ToolCallResult
			require.NoError(t, json.Unmarshal(data, &decoded))
			msg.ToolResult = &decoded
			restored, ok := editFileDiff(msg)
			require.True(t, ok)
			assert.Equal(t, meta, restored)

			added, removed := countDiffLines(msg)
			assert.Equal(t, tt.added, added)
			assert.Equal(t, tt.removed, removed)
			for _, split := range []bool{false, true} {
				before := renderEditFile(msg, 120, split)
				require.NoError(t, os.WriteFile(path, []byte("unrelated later contents\n"), 0o644))
				InvalidateCaches()
				assert.Equal(t, before, renderEditFile(msg, 120, split))
				require.NoError(t, os.Remove(path))
				InvalidateCaches()
				assert.Equal(t, before, renderEditFile(msg, 120, split))
				assert.NotContains(t, ansi.Strip(before), "unrelated later contents")
				if tt.added+tt.removed > 0 {
					assert.NotEmpty(t, before)
				}
			}
		})
	}
}

func TestRenderEditFileConfirmationAppliesAllEditsInOrder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "main.go")
	before := "package main\n\nimport (\n\t\"context\"\n\t\"fmt\"\n)\n"
	edits := []filesystem.Edit{{OldText: "\t\"context\"\n", NewText: "\t\"intermediate\"\n"}, {OldText: "\t\"intermediate\"\n"}}
	msg := editMessage(t, path, before, edits)
	expected, ok := editFileDiff(msg)
	require.True(t, ok)
	require.NoError(t, os.WriteFile(path, []byte(before), 0o644))
	msg.ToolStatus = types.ToolStatusConfirmation
	msg.ToolResult = nil
	actual, ok := editFileDiff(msg)
	require.True(t, ok)
	assert.Equal(t, expected, actual)
	preview := ansi.Strip(renderEditFile(msg, 120, false))
	assert.Contains(t, preview, "context")
	assert.NotContains(t, preview, "intermediate")
	// Unified context after the deletion uses the new file's line numbers.
	assert.Contains(t, preview, "   4 ")
}

func TestRenderEditFileDoesNotReconstructMissingMetadata(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "main.go")
	msg := editMessage(t, path, "old\n", []filesystem.Edit{{OldText: "old", NewText: "new"}})
	require.NoError(t, os.WriteFile(path, []byte("new\n"), 0o644))
	msg.ToolResult = nil
	assert.Empty(t, renderEditFile(msg, 120, false))
	// A result arriving without a status change must invalidate the preview cache.
	meta, err := filesystem.NewEditFileMeta(path, "old\n", "new\n")
	require.NoError(t, err)
	msg.ToolResult = &tools.ToolCallResult{Meta: meta}
	assert.NotEmpty(t, renderEditFile(msg, 120, false))
}

func TestEditFileViewFallsBackToToolHeaderWhenArgumentsCannotParse(t *testing.T) {
	t.Parallel()
	msg := types.ToolCallMessage("agent", tools.ToolCall{ID: t.Name(), Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path": "/tmp/file",`}}, tools.Tool{Name: "edit_file"}, types.ToolStatusPending)
	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	_ = view.SetSize(80, 0)
	assert.Contains(t, ansi.Strip(view.View()), "edit_file")
}

func TestRenderEditFileTruncatedDiff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, before, after string
	}{
		{"context omitted", strings.Repeat("x", filesystem.MaxEditFileMetaBytes) + "\nold\n", strings.Repeat("x", filesystem.MaxEditFileMetaBytes) + "\nnew\n"},
		{"change omitted", "old\n", strings.Repeat("x", filesystem.MaxEditFileMetaBytes) + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, err := filesystem.NewEditFileMeta("file.txt", tt.before, tt.after)
			require.NoError(t, err)
			require.True(t, meta.Truncated)
			msg := &types.Message{ToolCall: tools.ToolCall{ID: t.Name()}, ToolStatus: types.ToolStatusCompleted, ToolResult: &tools.ToolCallResult{Meta: meta}}
			for _, split := range []bool{false, true} {
				assert.Contains(t, ansi.Strip(renderEditFile(msg, 120, split)), "Diff truncated")
			}
			added, removed := countDiffLines(msg)
			assert.Equal(t, 1, added)
			assert.Equal(t, 1, removed)
		})
	}
}

func TestPairDiffLinesMultilineReplacement(t *testing.T) {
	t.Parallel()
	lines := []udiff.Line{
		{Kind: udiff.Delete, Content: "old first\n"},
		{Kind: udiff.Delete, Content: "old second\n"},
		{Kind: udiff.Insert, Content: "new first\n"},
		{Kind: udiff.Insert, Content: "new second\n"},
		{Kind: udiff.Insert, Content: "extra\n"},
		{Kind: udiff.Equal, Content: "context\n"},
	}
	pairs := pairDiffLines(lines, 10, 10)
	require.Len(t, pairs, 4)
	for i := range 2 {
		assert.Same(t, &lines[i], pairs[i].old)
		assert.Same(t, &lines[i+2], pairs[i].new)
		assert.Equal(t, 10+i, pairs[i].oldLineNum)
		assert.Equal(t, 10+i, pairs[i].newLineNum)
	}
	assert.Nil(t, pairs[2].old)
	assert.Same(t, &lines[4], pairs[2].new)
	assert.Equal(t, 12, pairs[3].oldLineNum)
	assert.Equal(t, 13, pairs[3].newLineNum)
	wordDiffs := buildLineWordDiffs(lines)
	assert.Len(t, wordDiffs, 4)
	for i := range 4 {
		assert.Contains(t, wordDiffs, i)
	}
}

func TestEditFileViewShowsDiffWhenPostEditCommandFails(t *testing.T) {
	t.Parallel()
	msg := editMessage(t, "/no/local/file.go", "old\n", []filesystem.Edit{{OldText: "old", NewText: "new"}})
	msg.ToolStatus = types.ToolStatusError
	msg.Content = "post-edit command failed"
	msg.ToolResult.IsError = true
	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	_ = view.SetSize(120, 0)
	output := ansi.Strip(view.View())
	assert.Contains(t, output, "post-edit command failed")
	assert.Contains(t, output, "old")
	assert.Contains(t, output, "new")
	assert.NotContains(t, output, "Edit #")
}
