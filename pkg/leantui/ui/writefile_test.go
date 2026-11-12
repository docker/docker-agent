package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/styles"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestHighlightFileLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path    string
		content string
		colored bool
	}{
		{"main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}", true},
		{"script.py", "def greet():\n    return \"hello\"", true},
		{"config.json", "{\"enabled\": true}", true},
		{"Dockerfile", "FROM alpine\nRUN echo hello", true},
		{"partial.go", "package main\nvar text = \"incomplete", true},
		{"file.unknown-extension", "plain text\n  stays plain", false},
		{"empty.go", "", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			highlighted := strings.Join(highlightFileLines(tc.content, tc.path), "\n")
			assert.Equal(t, tc.content, ansi.Strip(highlighted))
			assert.Equal(t, tc.colored, strings.Contains(highlighted, "\x1b["))
		})
	}
}

func TestWriteFileHighlightedTailPreservesMultilineContext(t *testing.T) {
	t.Parallel()
	content := "package main\n/*\n" + strings.Repeat("comment line\n", 12) + "*/"
	raw, err := json.Marshal(map[string]string{"path": "main.go", "content": content})
	require.NoError(t, err)
	view := NewToolView("root", tools.ToolCall{Function: tools.FunctionCall{Name: "write_file", Arguments: string(raw)}}, tools.Tool{Name: "write_file"}, tuitypes.ToolStatusPending)
	highlighted := strings.Join(highlightFileLines(content, "main.go"), "\n")
	rendered := strings.Join(RenderToolWithState(view, 80, 0, nil), "\n")
	commentColor := styles.ChromaStyle().Get(chroma.CommentMultiline).Colour.String()
	coloredComment := lipgloss.NewStyle().Foreground(lipgloss.Color(commentColor)).Italic(styles.ChromaStyle().Get(chroma.CommentMultiline).Italic == chroma.Yes).Render("comment line")
	assert.Contains(t, highlighted, coloredComment)
	assert.Equal(t, 9, strings.Count(ansi.Strip(rendered), "comment line"))
	assert.NotContains(t, ansi.Strip(rendered), "package main")
}

func TestWriteFileHighlightedPreviewWraps(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]string{"path": "main.go", "content": "package main\nvar message = \"a long string that wraps across several rows\""})
	require.NoError(t, err)
	view := NewToolView("root", tools.ToolCall{Function: tools.FunctionCall{Name: "write_file", Arguments: string(raw)}}, tools.Tool{Name: "write_file"}, tuitypes.ToolStatusPending)
	lines := RenderToolWithState(view, 24, 0, nil)
	for _, line := range lines {
		assert.LessOrEqual(t, DisplayWidth(line), 24)
		assert.NotContains(t, ansi.Strip(line), "\x1b")
	}
	assert.Contains(t, ansi.Strip(strings.Join(lines, "\n")), "package main")
}
