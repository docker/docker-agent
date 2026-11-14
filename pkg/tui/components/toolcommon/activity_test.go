package toolcommon

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSummarizeActivity(t *testing.T) {
	t.Parallel()
	assert.Empty(t, SummarizeActivity(nil, 80))
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "read_file"}}, tools.Tool{Name: "read_file"}, types.ToolStatusCompleted)
	messages := []*types.Message{msg, msg}
	text := ansi.Strip(SummarizeActivity(messages, 100))
	assert.Contains(t, text, "2 tool calls")
	assert.Contains(t, text, "read_file ×2")
	for _, width := range []int{1, 5, 20, 100} {
		assert.LessOrEqual(t, ansi.StringWidth(SummarizeActivity(messages, width)), width)
	}
	for _, status := range []types.ToolStatus{types.ToolStatusError, types.ToolStatusConfirmation} {
		msg.ToolStatus = status
		assert.False(t, IsRoutineActivity(msg))
	}
	assert.False(t, IsRoutineActivity(nil))
	edit := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "edit_file"}}, tools.Tool{Name: "edit_file"}, types.ToolStatusCompleted)
	assert.False(t, IsRoutineActivity(edit))
	for _, expanded := range []bool{false, true} {
		for _, width := range []int{1, 5, 20, 100} {
			assert.LessOrEqual(t, ansi.StringWidth(ActivityHeader(messages, width, expanded)), width)
		}
	}
	msg.ToolStatus = types.ToolStatusRunning
	msg.ToolCall.Function.Arguments = `{"description":"Reading\nfiles"}`
	assert.NotContains(t, SummarizeActivity(messages, 100), "\n")
	assert.Contains(t, ansi.Strip(SummarizeActivity(messages, 100)), "Reading files")
}
