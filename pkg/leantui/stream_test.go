package leantui

import (
	"bufio"
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/service"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

// bareModel builds a model with just the pieces buildLines needs, so the
// rendering pipeline can be exercised without a real App or terminal.
func bareModel(height int) *model {
	const width = 80

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	return &model{
		width:        width,
		height:       height,
		r:            ui.NewRenderer(w, width, height),
		screen:       ui.NewScreen("/tmp/project", "", "type here"),
		status:       ui.StatusModel{WorkingDir: "/tmp/project"},
		sessionState: service.NewSessionState(nil),
		usage:        ui.NewUsageTracker(),
	}
}

func TestStreamingGrowthScrollsAndRendersMarkdown(t *testing.T) {
	t.Parallel()
	m := bareModel(10)
	m.busy = true
	m.render() // initial frame

	m.screen.Transcript.AppendAssistant("")
	m.screen.Transcript.AppendAssistant("init")
	for i := range 40 {
		m.screen.Transcript.AppendAssistant("Paragraph " + strconv.Itoa(i) + " with some streamed text.\n\n")
		lines, cl, cc := m.buildLines()
		require.NotPanics(t, func() { m.r.Frame(lines, cl, cc) })
	}

	// Content far exceeds the 10-row viewport, so it must have scrolled.
	assert.Positive(t, m.r.ViewportTop())

	// Finalizing the stream turns it into a cached block; the visible output is
	// unchanged because it was already rendered as markdown live.
	m.screen.Transcript.FlushPending()
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
	require.NotPanics(t, func() {
		lines, cl, cc := m.buildLines()
		m.r.Frame(lines, cl, cc)
	})
}

func TestBuildLinesPlacesCursorOnInput(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Editor.SetText("hello")

	lines, cursorLine, cursorCol := m.buildLines()
	require.NotEmpty(t, lines)
	// The cursor line must point at the input row and the column past the prompt.
	assert.Contains(t, lines[cursorLine], "hello")
	assert.Equal(t, ui.PromptWidth+5, cursorCol)
}

func TestBuildLinesShowsSpinnerWhenBusy(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.busy = true
	lines, cursorLine, _ := m.buildLines()
	assert.Contains(t, lines[cursorLine-1], "Working")
}

func TestToolConfirmationReplacesRunningTool(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	tv := shellToolView(tuitypes.ToolStatusRunning)
	m.screen.Transcript.UpsertTool("root", tv.Message().ToolCall, tv.Message().ToolDefinition, tuitypes.ToolStatusRunning)
	require.Equal(t, 1, m.screen.Transcript.ToolCount())

	event := runtime.ToolCallConfirmation(tv.Message().ToolCall, tv.Message().ToolDefinition, "root", nil)
	m.handleEvent(t.Context(), event)

	assert.Zero(t, m.screen.Transcript.ToolCount())
	assert.Zero(t, m.screen.Transcript.ToolByIDCount())
	require.NotNil(t, m.screen.Confirm)
}

func TestBuildLinesConfirmCursorSitsOnOptions(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Confirm = &ui.ConfirmModel{
		Tool: "shell",
		View: *shellToolView(tuitypes.ToolStatusConfirmation),
	}

	lines, cursorLine, cursorCol := m.buildLines()
	require.NotEmpty(t, lines)
	require.GreaterOrEqual(t, cursorLine, 0)
	require.Less(t, cursorLine, len(lines))
	assert.Contains(t, lines[cursorLine], "[y] yes")
	assert.Positive(t, cursorCol)
}
