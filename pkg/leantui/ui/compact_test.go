package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestCompactTranscript(t *testing.T) {
	t.Parallel()
	transcript := NewTranscript()
	state := &service.SessionState{}
	state.SetCompactActivity(true)
	transcript.AppendReasoning("private reasoning")
	transcript.FlushPending()
	for _, id := range []string{"one", "two"} {
		transcript.UpsertTool("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"secret.txt"}`}}, tools.Tool{Name: "read_file"}, tuitypes.ToolStatusRunning)
		transcript.FinishTool(id, ToolResult{Response: "verbose result", Result: tools.ResultSuccess("verbose result")}, state)
	}
	render := func() string { return ansi.Strip(strings.Join(transcript.Lines(100, 0, false, state, nil), "\n")) }
	assert.Contains(t, render(), "2 tool calls")
	assert.NotContains(t, render(), "verbose result")
	assert.NotContains(t, render(), "private reasoning")
	assert.True(t, transcript.ToggleActivityAt(0))
	expanded := render()
	assert.Contains(t, expanded, "private reasoning")
	assert.Contains(t, expanded, "secret.txt")
	assert.Less(t, strings.Index(expanded, "private reasoning"), strings.Index(expanded, "secret.txt"))
	assert.True(t, transcript.ToggleActivityAt(0))
	assert.NotContains(t, render(), "private reasoning")
	transcript.UpsertTool("root", tools.ToolCall{ID: "fail", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{Name: "shell"}, tuitypes.ToolStatusRunning)
	transcript.FinishTool("fail", ToolResult{Response: "permission denied", Result: tools.ResultError("permission denied")}, state)
	assert.Contains(t, render(), "permission denied")
	transcript.UpsertTool("root", tools.ToolCall{ID: "edit", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path":"always-visible.txt","oldText":"before","newText":"after"}`}}, tools.Tool{Name: "edit_file"}, tuitypes.ToolStatusRunning)
	assert.Contains(t, render(), "always-visible.txt")
	transcript.FinishTool("edit", ToolResult{Result: tools.ResultSuccess("done")}, state)
	assert.Contains(t, render(), "always-visible.txt")
	transcript.AppendAssistant("Final answer")
	assert.Contains(t, render(), "Final answer")
	state.SetCompactActivity(false)
	assert.Contains(t, render(), "secret.txt")
	assert.Contains(t, render(), "private reasoning")
}

func TestCompactReasoningOnlyToggle(t *testing.T) {
	t.Parallel()
	transcript := NewTranscript()
	state := &service.SessionState{}
	state.SetCompactActivity(true)
	transcript.AppendReasoning("First thought")
	render := func() string { return ansi.Strip(strings.Join(transcript.Lines(80, 0, true, state, nil), "\n")) }
	assert.Contains(t, render(), "[+] Thinking")
	assert.True(t, transcript.ToggleActivityAt(0))
	assert.Contains(t, render(), "First thought")
	transcript.AppendReasoning(" and more")
	assert.Contains(t, render(), "First thought and more")
	transcript.FlushPending()
	assert.Contains(t, render(), "First thought and more", "expansion survives finalization")
	assert.True(t, transcript.ToggleActivityAt(0))
	assert.NotContains(t, render(), "First thought")
}

func TestExpandedCompletedActivityUsesBlockCache(t *testing.T) {
	t.Parallel()
	transcript, state := compactTestTranscript(t)
	transcript.Lines(80, 0, false, state, nil)
	assert.True(t, transcript.ToggleActivityAt(0))
	calls := 0
	b := transcript.blocks[0]
	render := b.render
	b.render = func(w int) []string { calls++; return render(w) }
	for frame := range 3 {
		transcript.Lines(80, frame, false, state, nil)
	}
	assert.Equal(t, 1, calls)
	transcript.Lines(60, 0, false, state, nil)
	assert.Equal(t, 2, calls)
}

func TestCompactLiveActivity(t *testing.T) {
	t.Parallel()
	transcript := NewTranscript()
	state := &service.SessionState{}
	state.SetCompactActivity(true)
	for _, id := range []string{"one", "two"} {
		transcript.UpsertTool("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "shell", Arguments: `{"description":"Checking tests"}`}}, tools.Tool{Name: "shell"}, tuitypes.ToolStatusRunning)
	}
	text := ansi.Strip(strings.Join(transcript.Lines(80, 0, true, state, nil), "\n"))
	assert.Contains(t, text, "2 running")
	assert.Contains(t, text, "Checking tests")
	assert.LessOrEqual(t, len(strings.Split(strings.TrimSpace(text), "\n")), 1)
	assert.True(t, transcript.ToggleActivityAt(0))
	transcript.UpsertTool("root", tools.ToolCall{ID: "three", Function: tools.FunctionCall{Name: "shell", Arguments: `{"command":"visible command"}`}}, tools.Tool{Name: "shell"}, tuitypes.ToolStatusRunning)
	transcript.FinishTool("one", ToolResult{Result: tools.ResultError("failed")}, state)
	transcript.FinishTool("two", ToolResult{Result: tools.ResultError("failed")}, state)
	assert.True(t, transcript.expandedActivity["three"], "new members inherit expansion before rendering")
	assert.Contains(t, strings.Join(transcript.Lines(80, 0, true, state, nil), "\n"), "[-]", "surviving calls retain expansion")
}
