package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestTranscriptToolsKeepInvocationOrder(t *testing.T) {
	t.Parallel()
	transcript := NewTranscript()
	state := service.StaticSessionState{}
	for _, name := range []string{"javascript_parent", "inner_first", "inner_second"} {
		transcript.UpsertTool("root", tools.ToolCall{ID: name, Function: tools.FunctionCall{Name: name, Arguments: `{"value":"test"}`}}, tools.Tool{Name: name}, tuitypes.ToolStatusRunning)
	}
	assertOrder := func() {
		t.Helper()
		text := ansi.Strip(strings.Join(transcript.Lines(100, 0, false, state, nil), "\n"))
		parent := strings.Index(text, "javascript_parent")
		first := strings.Index(text, "inner_first")
		second := strings.Index(text, "inner_second")
		require.NotEqual(t, -1, parent)
		require.NotEqual(t, -1, first)
		require.NotEqual(t, -1, second)
		assert.Less(t, parent, first)
		assert.Less(t, first, second)
	}
	assertOrder()
	transcript.FinishTool("inner_second", ToolResult{ToolDefinition: tools.Tool{Name: "inner_second"}, Result: tools.ResultError("failed")}, state)
	assertOrder()
	assert.Zero(t, transcript.BlockCount())
	transcript.FinishTool("inner_first", ToolResult{ToolDefinition: tools.Tool{Name: "inner_first"}, Result: tools.ResultSuccess("done")}, state)
	assertOrder()
	assert.Zero(t, transcript.BlockCount())
	transcript.FinishTool("javascript_parent", ToolResult{ToolDefinition: tools.Tool{Name: "javascript_parent"}, Result: tools.ResultSuccess("done")}, state)
	assertOrder()
	assert.Equal(t, 3, transcript.BlockCount())
	assert.Zero(t, transcript.ToolCount())
}

func TestFinalizeToolsPreservesCompletedResults(t *testing.T) {
	t.Parallel()
	transcript := NewTranscript()
	for _, name := range []string{"parent", "child"} {
		transcript.UpsertTool("root", tools.ToolCall{ID: name, Function: tools.FunctionCall{Name: name, Arguments: `{"value":"test"}`}}, tools.Tool{Name: name}, tuitypes.ToolStatusRunning)
	}
	transcript.FinishTool("child", ToolResult{Response: "child succeeded", ToolDefinition: tools.Tool{Name: "child"}}, nil)
	require.Equal(t, tuitypes.ToolStatusCompleted, transcript.Tool("child").Message().ToolStatus)
	transcript.FinalizeTools(tuitypes.ToolStatusError, nil)
	assert.Equal(t, 2, transcript.BlockCount())
	assert.Zero(t, transcript.ToolCount())
	assert.Contains(t, strings.Join(transcript.BlockLines(1, 100), "\n"), "child succeeded")
}
