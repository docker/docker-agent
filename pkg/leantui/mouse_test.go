package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestScrollExpandedActivityToCollapse(t *testing.T) {
	t.Parallel()
	m := bareModel(12)
	m.sessionState.SetCompactActivity(true)
	for i := range 8 {
		id := strings.Repeat("x", i+1)
		m.screen.Transcript.UpsertTool("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"secret.txt"}`}}, tools.Tool{Name: "read_file"}, tuitypes.ToolStatusRunning)
	}
	m.render()
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyMouseClick, X: 0, Y: 0})
	m.render()
	require.Positive(t, m.r.ViewportTop())
	for range 100 {
		m.handleKey(t.Context(), ui.Key{Typ: ui.KeyMouseWheelUp})
		m.render()
	}
	require.Zero(t, m.r.ViewportTop())
	m.spinnerFrame++
	m.render()
	assert.Zero(t, m.r.ViewportTop(), "animation must not jump to the bottom")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyMouseClick, X: 0, Y: 0})
	m.render()
	lines, _, _ := m.buildLines()
	assert.NotContains(t, ansi.Strip(strings.Join(lines, "\n")), "secret.txt")
	assert.Empty(t, m.screen.Editor.Text())
}

func TestScrolledApprovalRequiresVisiblePrompt(t *testing.T) {
	t.Parallel()
	m := bareModel(12)
	m.sessionState.SetCompactActivity(true)
	m.screen.Transcript.AddBlock(func(int) []string { return strings.Split(strings.Repeat("history\n", 30), "\n") })
	m.render()
	m.r.Scroll(-20)
	m.render()
	event := runtime.ToolCallConfirmation(tools.ToolCall{ID: "approval", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{Name: "shell"}, "root", nil)
	m.handleEvent(t.Context(), event)
	m.render()
	lines, cursorLine, _ := m.buildLines()
	assert.Less(t, cursorLine-m.r.ViewportTop(), m.height)
	assert.Contains(t, strings.Join(lines[m.r.ViewportTop():], "\n"), "Approve tool call")
	for _, key := range []rune{'y', 'a', 'b', 's'} {
		m.r.Scroll(-20)
		m.render()
		m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{key}})
		require.NotNil(t, m.screen.Confirm, "returning from scroll must not approve")
		m.render()
	}
}

func TestClickCompactActivity(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.sessionState.SetCompactActivity(true)
	for _, id := range []string{"one", "two"} {
		m.screen.Transcript.UpsertTool("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"secret.txt"}`}}, tools.Tool{Name: "read_file"}, tuitypes.ToolStatusRunning)
	}
	m.render()
	clickSummary := func() {
		t.Helper()
		lines, _, _ := m.buildLines()
		row := -1
		for i := m.r.ViewportTop(); i < len(lines) && i < m.r.ViewportTop()+m.height; i++ {
			if strings.Contains(ansi.Strip(lines[i]), "2 tool calls") || strings.Contains(ansi.Strip(lines[i]), "2 running") {
				row = i - m.r.ViewportTop()
			}
		}
		require.GreaterOrEqual(t, row, 0)
		m.handleKey(t.Context(), ui.Key{Typ: ui.KeyMouseClick, X: 0, Y: row})
		m.render()
	}
	text := func() string { lines, _, _ := m.buildLines(); return ansi.Strip(strings.Join(lines, "\n")) }
	assert.NotContains(t, text(), "secret.txt")
	clickSummary()
	assert.Contains(t, text(), "secret.txt")
	for _, id := range []string{"two", "one"} {
		m.screen.Transcript.FinishTool(id, ui.ToolResult{Result: tools.ResultSuccess("done")}, m.sessionState)
	}
	m.render()
	assert.Contains(t, text(), "secret.txt", "expansion survives tools moving to committed blocks")
	clickSummary()
	assert.NotContains(t, text(), "secret.txt")
	assert.Empty(t, m.screen.Editor.Text())
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyMouseClick, X: 0, Y: m.height - 1})
	assert.NotContains(t, text(), "secret.txt")
}
