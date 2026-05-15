package reasoningblock

import (
	"strconv"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestReasoningBlockShowsAllReasoningByDefault(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)
	block.SetReasoning("Line 1\nLine 2\nLine 3\nLine 4\nLine 5\nLine 6")

	assert.True(t, block.IsExpanded())

	stripped := ansi.Strip(block.View())
	assert.Contains(t, stripped, "Thinking")
	assert.Contains(t, stripped, "Line 1")
	assert.Contains(t, stripped, "Line 6")
	assert.NotContains(t, stripped, "[+]")
	assert.NotContains(t, stripped, "[-]")
}

func TestReasoningBlockAlwaysShowsToolCalls(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)
	block.SetReasoning("Thinking...")

	for i, status := range []types.ToolStatus{
		types.ToolStatusRunning,
		types.ToolStatusCompleted,
		types.ToolStatusError,
	} {
		name := "tool_" + strconv.Itoa(i)
		toolMsg := types.ToolCallMessage("root", tools.ToolCall{
			ID:       "call-" + strconv.Itoa(i),
			Function: tools.FunctionCall{Name: name, Arguments: "{}"},
		}, tools.Tool{Name: name}, status)
		block.AddToolCall(toolMsg)
	}

	stripped := ansi.Strip(block.View())
	assert.Contains(t, stripped, "Thinking...")
	assert.Contains(t, stripped, "tool_0")
	assert.Contains(t, stripped, "tool_1")
	assert.Contains(t, stripped, "tool_2")
	assert.NotContains(t, stripped, "3 tools")
}

func TestReasoningBlockShowsCompletedToolAddedFromHistory(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)
	block.SetReasoning("Thinking...")

	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "restored_tool", Arguments: `{}`},
	}, tools.Tool{Name: "restored_tool", Description: "A restored tool"}, types.ToolStatusCompleted)
	block.AddToolCall(toolMsg)

	stripped := ansi.Strip(block.View())
	assert.Contains(t, stripped, "restored_tool")
}

func TestReasoningBlockToggleIsDisabled(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)
	block.SetReasoning("Some reasoning")

	assert.True(t, block.IsExpanded())
	assert.False(t, block.IsToggleLine(0))

	block.Toggle()
	assert.True(t, block.IsExpanded())
	assert.Contains(t, ansi.Strip(block.View()), "Some reasoning")

	block.SetExpanded(false)
	assert.True(t, block.IsExpanded())
	assert.Contains(t, ansi.Strip(block.View()), "Some reasoning")
}

func TestReasoningBlockAppendReasoning(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)

	block.SetReasoning("First part")
	assert.Equal(t, "First part", block.Reasoning())

	block.AppendReasoning(" second part")
	assert.Equal(t, "First part second part", block.Reasoning())
}

func TestReasoningBlockInterleavesReasoningAndTools(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)

	block.SetReasoning("Before tool")
	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`},
	}, tools.Tool{Name: "read_file"}, types.ToolStatusCompleted)
	block.AddToolCall(toolMsg)
	block.AppendReasoning("After tool")

	stripped := ansi.Strip(block.View())
	beforeIdx := assert.Contains(t, stripped, "Before tool")
	toolIdx := assert.Contains(t, stripped, "read_file")
	afterIdx := assert.Contains(t, stripped, "After tool")
	assert.True(t, beforeIdx && toolIdx && afterIdx)
	assert.Less(t, indexOf(stripped, "Before tool"), indexOf(stripped, "read_file"))
	assert.Less(t, indexOf(stripped, "read_file"), indexOf(stripped, "After tool"))
}

func TestReasoningBlockUpdateToolCall(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)

	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "test_tool", Arguments: "{}"},
	}, tools.Tool{Name: "test_tool"}, types.ToolStatusPending)
	block.AddToolCall(toolMsg)

	block.UpdateToolCall("call-1", types.ToolStatusCompleted, `{"result": "done"}`)

	assert.True(t, block.HasToolCall("call-1"))
	assert.Contains(t, ansi.Strip(block.View()), "test_tool")
}

func TestReasoningBlockUpdateToolResult(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)

	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "test_tool", Arguments: "{}"},
	}, tools.Tool{Name: "test_tool"}, types.ToolStatusRunning)
	block.AddToolCall(toolMsg)

	result := &tools.ToolCallResult{Output: "Success!"}
	block.UpdateToolResult("call-1", "Success!", types.ToolStatusCompleted, result)

	assert.True(t, block.HasToolCall("call-1"))
	stripped := ansi.Strip(block.View())
	assert.Contains(t, stripped, "test_tool")
}

func TestReasoningBlockNeedsTickOnlyForActiveTools(t *testing.T) {
	t.Parallel()

	block := New("test-1", "root", &service.SessionState{})
	block.SetSize(80, 24)
	block.SetReasoning("Thinking...")

	assert.False(t, block.NeedsTick())

	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "test_tool", Arguments: `{}`},
	}, tools.Tool{Name: "test_tool"}, types.ToolStatusRunning)
	block.AddToolCall(toolMsg)
	assert.True(t, block.NeedsTick())

	result := &tools.ToolCallResult{Output: "Done!"}
	block.UpdateToolResult("call-1", "Done!", types.ToolStatusCompleted, result)
	assert.False(t, block.NeedsTick())
}

func TestReasoningBlockID(t *testing.T) {
	t.Parallel()

	block := New("test-block-123", "root", &service.SessionState{})
	assert.Equal(t, "test-block-123", block.ID())
}

func indexOf(s, substr string) int {
	for i := range s {
		if len(s[i:]) >= len(substr) && s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
