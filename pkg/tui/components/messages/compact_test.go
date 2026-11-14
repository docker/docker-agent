package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestExpandedActivityCachesCompletedDetails(t *testing.T) {
	t.Parallel()
	state := &service.SessionState{}
	state.SetCompactActivity(true)
	m := NewScrollableView(animation.NewRuntime(), 100, 40, state).(*model)
	for _, id := range []string{"done", "live"} {
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "example"}}, tools.Tool{Name: "example"}, types.ToolStatusRunning)
	}
	m.messages[0].SetToolStatus(types.ToolStatusCompleted)
	m.views[0], m.views[1] = &dynamicView{}, &dynamicView{}
	m.expandedActivity = map[string]bool{"done": true}
	assert.Contains(t, strings.Join(m.renderItem(1, m.views[1]).lines, "\n"), "frame-0")
	m.views[0].(*dynamicView).frame++
	m.views[1].(*dynamicView).frame++
	text := strings.Join(m.renderItem(1, m.views[1]).lines, "\n")
	assert.Contains(t, text, "frame-0", "completed details are cached")
	assert.Contains(t, text, "frame-1", "live details are rerendered")
	m.invalidateItem(0)
	assert.NotContains(t, strings.Join(m.renderItem(1, m.views[1]).lines, "\n"), "frame-0")
	assert.NotContains(t, strings.Join(m.renderItem(1, m.views[1]).lines, "\n"), "frame-1\n\nframe-1")
	m.messages[1].ToolCall.Function.Name = "transfer_task"
	assert.Contains(t, strings.Join(m.renderItem(1, m.views[1]).lines, "\n"), "frame-1\n\nframe-1", "delegation retains its normal separator")
}

func TestCompactActivityGroupsAndRestoresDetails(t *testing.T) {
	t.Parallel()
	state := &service.SessionState{}
	state.SetCompactActivity(true)
	m := NewScrollableView(animation.NewRuntime(), 100, 40, state).(*model)
	for _, id := range []string{"one", "two"} {
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "example", Arguments: `{"value":"hidden arguments"}`}}, tools.Tool{Name: "example"}, types.ToolStatusCompleted)
	}
	render := func() string {
		m.updateScrollState()
		return ansi.Strip(strings.Join(m.renderedLines, "\n"))
	}
	assert.Contains(t, render(), "2 tool calls")
	assert.NotContains(t, render(), "hidden arguments")
	clickSummary := func() {
		m.updateScrollState()
		_, _ = m.handleMouseClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: m.xPos, Y: m.yPos + m.lineOffsets[1] - m.scrollOffset})
	}
	clickSummary()
	assert.Contains(t, render(), "[-]")
	assert.Contains(t, render(), "hidden arguments")
	clickSummary()
	assert.Contains(t, render(), "[+]")
	assert.NotContains(t, render(), "hidden arguments")
	clickSummary()
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "three", Function: tools.FunctionCall{Name: "example", Arguments: `{}`}}, tools.Tool{Name: "example"}, types.ToolStatusCompleted)
	assert.Contains(t, render(), "3 tool calls")
	assert.Contains(t, render(), "hidden arguments")
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "one", Function: tools.FunctionCall{Name: "example"}}, tools.Tool{Name: "example"}, types.ToolStatusError)
	assert.Contains(t, render(), "2 tool calls")
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "two", Function: tools.FunctionCall{Name: "example"}}, tools.Tool{Name: "example"}, types.ToolStatusError)
	assert.True(t, m.expandedActivity["three"], "new members retain expansion after earlier members fail")
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "edit", Function: tools.FunctionCall{Name: "edit_file", Arguments: `{"path":"always-visible.txt","oldText":"before","newText":"after"}`}}, tools.Tool{Name: "edit_file"}, types.ToolStatusCompleted)
	assert.Contains(t, render(), "always-visible.txt")
	require.NotEmpty(t, m.renderedLines)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "fail", Function: tools.FunctionCall{Name: "failed_tool"}}, tools.Tool{Name: "failed_tool"}, types.ToolStatusError)
	assert.Contains(t, render(), "failed_tool")
	state.SetCompactActivity(false)
	m.Update(tuimessages.ActivityDisplayChangedMsg{})
	assert.NotContains(t, render(), "2 tool calls")
	assert.Contains(t, render(), "hidden arguments")
}
