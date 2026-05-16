package dialog

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

var utf8Valid = utf8.Valid

func TestNewToolsDialog_EmptyShowsBothPlaceholders(t *testing.T) {
	t.Parallel()
	d := NewToolsDialog(nil, nil).(*toolsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	assert.Contains(t, out, "Tools (0 toolsets · 0 tools)")
	assert.Contains(t, out, "No toolsets configured")
	assert.Contains(t, out, "No tools available")
}

func TestNewToolsDialog_RendersToolsetSection(t *testing.T) {
	t.Parallel()

	statuses := []tools.ToolsetStatus{
		{
			Name:  "gopls",
			Kind:  "LSP",
			State: lifecycle.StateReady,
		},
		{
			Name:         "github-mcp",
			Kind:         "Remote MCP",
			State:        lifecycle.StateRestarting,
			LastError:    errors.New("connection reset"),
			RestartCount: 2,
		},
	}
	d := NewToolsDialog(statuses, nil).(*toolsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	assert.Contains(t, out, "Tools (2 toolsets · 0 tools)")
	assert.Contains(t, out, "gopls")
	assert.Contains(t, out, "ready")
	assert.Contains(t, out, "LSP")
	assert.Contains(t, out, "github-mcp")
	assert.Contains(t, out, "restarting")
	assert.Contains(t, out, "Remote MCP")
	assert.Contains(t, out, "connection reset")
	assert.Contains(t, out, "restarts: 2")
}

// TestNewToolsDialog_NoKindRendersBuiltInLabel guards against blank Kind
// rows leaving a hole where the label should be: built-in toolsets
// (memory, shell, filesystem, …) don't implement tools.Kinder, but they
// still need a visible label in the column.
func TestNewToolsDialog_NoKindRendersBuiltInLabel(t *testing.T) {
	t.Parallel()
	statuses := []tools.ToolsetStatus{{
		Name:  "memory",
		State: lifecycle.StateReady,
	}}
	d := NewToolsDialog(statuses, nil).(*toolsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	assert.Contains(t, out, "Built-in")
}

// TestNewToolsDialog_RendersToolsByCategory verifies that the lower
// "Tools" section groups items by their Category and shows the
// per-tool description suffix. The exact rendering is theme-dependent
// so we only assert on the substrings the user actually reads.
func TestNewToolsDialog_RendersToolsByCategory(t *testing.T) {
	t.Parallel()
	toolList := []tools.Tool{
		{Name: "fs_read", Category: "filesystem", Description: "Read a file"},
		{Name: "fs_write", Category: "filesystem", Description: "Write a file"},
		{Name: "shell", Category: "shell", Description: "Execute commands"},
	}
	d := NewToolsDialog(nil, toolList).(*toolsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	assert.Contains(t, out, "Tools (0 toolsets · 3 tools)")
	assert.Contains(t, out, "filesystem")
	assert.Contains(t, out, "shell")
	assert.Contains(t, out, "fs_read")
	assert.Contains(t, out, "Read a file")
	// Category headings should come before their tools in the buffer.
	assert.Less(t, strings.Index(out, "filesystem"), strings.Index(out, "fs_read"))
}

// TestFormatToolsetStatus_TruncatesLongErrors guards against blowing out the
// dialog with multi-kilobyte error messages from upstream stack traces.
func TestFormatToolsetStatus_TruncatesLongErrors(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 1000)
	statuses := []tools.ToolsetStatus{{
		Name:      "x",
		State:     lifecycle.StateFailed,
		LastError: errors.New(long),
	}}
	d := NewToolsDialog(statuses, nil).(*toolsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	// The "…" marker must appear because the error is truncated.
	assert.Contains(t, out, "…")
}

// TestFormatToolsetStatus_TruncatesAtRuneBoundary guards against
// byte-based truncation of multi-byte UTF-8 sequences (each emoji is
// 4 bytes; a byte-truncating algorithm would land mid-codepoint and
// produce invalid UTF-8 — lipgloss would then either panic or render
// a replacement character).
func TestFormatToolsetStatus_TruncatesAtRuneBoundary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("\U0001F600", 1000) // 1000 "😀" runes (4 bytes each)
	statuses := []tools.ToolsetStatus{{
		Name:      "x",
		State:     lifecycle.StateFailed,
		LastError: errors.New(long),
	}}
	d := NewToolsDialog(statuses, nil).(*toolsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	// Every byte in the output must be a valid UTF-8 sequence.
	assert.True(t, utf8ValidString(out), "truncated output must remain valid UTF-8")
	assert.Contains(t, out, "…")
}

func utf8ValidString(s string) bool {
	return utf8Valid([]byte(s))
}
