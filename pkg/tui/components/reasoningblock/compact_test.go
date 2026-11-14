package reasoningblock

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestCompactReasoningPreservesErrorsAndExpansion(t *testing.T) {
	t.Parallel()
	state := &service.SessionState{}
	state.SetExpandThinking(true)
	state.SetCompactActivity(true)
	block := New(animation.NewRuntime(), "compact", "root", state)
	block.SetSize(100, 40)
	block.SetReasoning("Detailed reasoning")
	assert.Contains(t, ansi.Strip(block.View()), "[+]")
	assert.True(t, block.IsToggleLine(block.headerLineIndex()))
	for _, id := range []string{"one", "two"} {
		block.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "example"}}, tools.Tool{Name: "example"}, types.ToolStatusCompleted))
	}
	block.AppendReasoning("After tools")
	assert.False(t, block.IsExpanded())
	text := ansi.Strip(block.View())
	assert.Contains(t, text, "2 tool calls")
	assert.NotContains(t, text, "Detailed reasoning")
	block.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: "fail", Function: tools.FunctionCall{Name: "failed_tool"}}, tools.Tool{Name: "failed_tool"}, types.ToolStatusError))
	assert.Contains(t, ansi.Strip(block.View()), "failed_tool")
	block.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: "edit", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path":"always-visible.txt","oldText":"before","newText":"after"}`}}, tools.Tool{Name: "edit_file"}, types.ToolStatusCompleted))
	assert.Contains(t, ansi.Strip(block.View()), "always-visible.txt")
	for _, expanded := range []bool{true, false} {
		for line, text := range strings.Split(ansi.Strip(block.View()), "\n") {
			if strings.Contains(text, "2 tool calls") {
				assert.True(t, block.IsToggleLine(line))
				block.Toggle()
				break
			}
		}
		assert.Equal(t, expanded, block.IsExpanded())
		if expanded {
			text := ansi.Strip(block.View())
			assert.Less(t, strings.Index(text, "Detailed reasoning"), strings.Index(text, "2 tool calls"))
			assert.Less(t, strings.Index(text, "2 tool calls"), strings.Index(text, "After tools"))
		} else {
			assert.NotContains(t, ansi.Strip(block.View()), "Detailed reasoning")
		}
		assert.Contains(t, ansi.Strip(block.View()), "always-visible.txt")
	}
	block.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: "last", Function: tools.FunctionCall{Name: "last_tool"}}, tools.Tool{Name: "last_tool"}, types.ToolStatusCompleted))
	block.SetExpanded(true)
	for line, text := range strings.Split(ansi.Strip(block.View()), "\n") {
		if strings.Contains(text, "[-]") {
			assert.True(t, block.IsToggleLine(line), "summary at line %d", line)
		}
	}
}
