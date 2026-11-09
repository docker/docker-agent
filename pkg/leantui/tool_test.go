package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestRenderToolOutputTruncatesOutput(t *testing.T) {
	t.Parallel()
	output := strings.Repeat("line\n", 50)
	lines := ui.RenderToolOutput(output, 80)

	assert.LessOrEqual(t, len(lines), ui.MaxToolOutputLines+1)
	assert.Contains(t, strings.Join(lines, "\n"), "earlier lines")
}

func TestRenderToolUsesFullTUIRenderer(t *testing.T) {
	t.Parallel()
	tv := shellToolView(tuitypes.ToolStatusCompleted)
	tv.Message().Content = "hi\n"

	joined := strings.Join(ui.RenderTool(*tv, 80), "\n")
	assert.Contains(t, joined, builtinshell.ToolNameShell)
	assert.Contains(t, joined, "echo hi")
	assert.Contains(t, joined, "hi")
	assert.NotContains(t, joined, "Took")
}

func TestRenderToolWrapsCallInBox(t *testing.T) {
	t.Parallel()
	width := 40
	lines := ui.RenderTool(*shellToolView(tuitypes.ToolStatusCompleted), width)
	require.GreaterOrEqual(t, len(lines), 3)

	for _, line := range lines {
		assert.LessOrEqual(t, ui.DisplayWidth(line), width)
	}
	assert.Empty(t, strings.TrimSpace(ansi.Strip(lines[0])))
	assert.Equal(t, width, ui.DisplayWidth(lines[0]))
	assert.True(t, strings.HasPrefix(ansi.Strip(lines[1]), " "))
	assert.Contains(t, ansi.Strip(strings.Join(lines, "\n")), builtinshell.ToolNameShell)
}

func TestRenderToolDoesNotLeakAnimationSubscription(t *testing.T) {
	ui.RenderToolWithState(shellToolView(tuitypes.ToolStatusRunning), 80, 3, nil)
	// RenderToolWithState owns a short-lived runtime and StopView cleanup. A
	// second render must remain safe rather than depending on package globals.
	ui.RenderToolWithState(shellToolView(tuitypes.ToolStatusRunning), 80, 4, nil)
}

func TestRenderToolKeepsLastLinesWhenArgumentsTemporarilyInvalid(t *testing.T) {
	tv := ui.NewToolView("root", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      "Write",
			Arguments: `{"path": "/tmp/file", "content": "hello"`,
		},
	}, tools.Tool{Name: "Write"}, tuitypes.ToolStatusPending)

	first := ui.RenderToolWithState(tv, 80, 0, nil)
	want := strings.Join(first, "\n")
	require.Contains(t, want, "hello")
	first[0] = "mutated first render"

	tv.Message().ToolCall.Function.Arguments += ","
	second := ui.RenderToolWithState(tv, 80, 1, nil)
	require.Equal(t, want, strings.Join(second, "\n"))
	second[0] = "mutated cached render"
	assert.Equal(t, want, strings.Join(ui.RenderToolWithState(tv, 80, 2, nil), "\n"))
}

func TestRenderWriteFileKeepsPathWhenArgumentsTemporarilyInvalid(t *testing.T) {
	tv := ui.NewToolView("root", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      filesystem.ToolNameWriteFile,
			Arguments: `{"path": "/tmp/file"`,
		},
	}, tools.Tool{Name: filesystem.ToolNameWriteFile}, tuitypes.ToolStatusPending)

	first := ui.RenderToolWithState(tv, 80, 0, nil)
	require.Contains(t, strings.Join(first, "\n"), "/tmp/file")

	tv.Message().ToolCall.Function.Arguments += ","
	second := ui.RenderToolWithState(tv, 80, 1, nil)
	assert.Contains(t, strings.Join(second, "\n"), "/tmp/file")
}

func shellToolView(status tuitypes.ToolStatus) *ui.ToolView {
	return ui.NewToolView("root", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      builtinshell.ToolNameShell,
			Arguments: `{"cmd":"echo hi"}`,
		},
	}, tools.Tool{Name: builtinshell.ToolNameShell}, status)
}
