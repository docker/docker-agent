package messages

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestBreakMessageGroupSeparatesSameAgentContent(t *testing.T) {
	t.Parallel()

	for _, reasoning := range []bool{false, true} {
		name := "text"
		if reasoning {
			name = "reasoning"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
			appendContent := m.AppendToLastMessage
			if reasoning {
				appendContent = m.AppendReasoning
			}
			appendContent("root", "child")
			appendContent("root", " reply")
			m.AddAssistantMessage("root", "Waiting")
			m.BreakMessageGroup()
			m.BreakMessageGroup()
			m.RemoveSpinner()
			m.AppendAssistantMedia("root", nil)
			appendContent("root", "parent")
			appendContent("root", " reply")

			require.Len(t, m.messages, 2)
			require.Equal(t, "child reply", m.messages[0].Content)
			require.Equal(t, "parent reply", m.messages[1].Content)
		})
	}
}

func TestBreakMessageGroupSeparatesMedia(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AppendToLastMessage("root", "child")
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 1, Fallback: "child image"}})
	m.BreakMessageGroup()
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 2, Fallback: "parent image"}})
	m.AppendToLastMessage("root", "parent")
	m.UpdateAssistantMedia([]types.AssistantMedia{{ID: 1, Fallback: "resolved child image"}})

	require.Len(t, m.messages, 2)
	require.Equal(t, "child", m.messages[0].Content)
	require.Equal(t, []types.AssistantMedia{{ID: 1, Fallback: "resolved child image"}}, m.messages[0].AssistantMedia)
	require.Equal(t, "parent", m.messages[1].Content)
	require.Equal(t, []types.AssistantMedia{{ID: 2, Fallback: "parent image"}}, m.messages[1].AssistantMedia)
}

func TestBreakMessageGroupSeparatesNewToolsButAllowsUpdates(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AppendReasoning("root", "child reasoning")
	oldCall := tools.ToolCall{ID: "old", Function: tools.FunctionCall{Name: "shell"}}
	m.AddOrUpdateToolCall("root", oldCall, tools.Tool{}, types.ToolStatusPending)
	m.BreakMessageGroup()
	m.AddOrUpdateToolCall("root", oldCall, tools.Tool{}, types.ToolStatusRunning)
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "old", Response: "done"}, types.ToolStatusCompleted)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "new", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)

	require.Len(t, m.messages, 2)
	require.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	require.Equal(t, types.MessageTypeToolCall, m.messages[1].Type)
	require.Equal(t, "new", m.messages[1].ToolCall.ID)
}

func TestBreakMessageGroupResetOnLoad(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AddUserMessage("old conversation")
	m.AppendToLastMessage("root", "old reply")
	m.BreakMessageGroup()
	m.LoadFromSession(session.New(), nil)
	m.AppendToLastMessage("root", "new")
	m.AppendToLastMessage("root", " reply")

	require.Len(t, m.messages, 1)
	require.Equal(t, "new reply", m.messages[0].Content)
}

func TestMessageGroupRebasedAfterPendingToolsRemoved(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AppendToLastMessage("root", "old reply")
	for i := range 4 {
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: strconv.Itoa(i), Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusRunning)
	}
	m.BreakMessageGroup()
	m.AppendToLastMessage("root", "new")
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "later", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)
	m.removePendingToolCallMessages()
	m.AppendToLastMessage("root", " reply")

	require.Equal(t, 1, m.groupStart)
	require.Len(t, m.messages, 2)
	require.Equal(t, "old reply", m.messages[0].Content)
	require.Equal(t, "new reply", m.messages[1].Content)
}

func TestMessageGroupRebasedAfterLoadingRemoved(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AddLoadingMessage("Loading command")
	m.AppendToLastMessage("root", "old reply")
	m.BreakMessageGroup()
	m.ReplaceLoadingWithUser("new question", 0)

	require.Equal(t, 1, m.groupStart)
	m.AppendToLastMessage("root", "new")
	m.AppendToLastMessage("root", " reply")
	require.Len(t, m.messages, 3)
	require.Equal(t, "new reply", m.messages[2].Content)
}

// Retracted tool calls disappear wherever they render, and only them.
func TestRemoveToolCalls(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AppendReasoning("root", "plan")
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "in-block", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "kept-in-block", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)
	m.BreakMessageGroup()
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "standalone", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "kept", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, types.ToolStatusPending)

	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok)
	require.Equal(t, 2, block.ToolCount())

	m.RemoveToolCalls([]string{"in-block", "standalone"})

	assert.False(t, block.HasToolCall("in-block"))
	assert.True(t, block.HasToolCall("kept-in-block"))
	assert.Equal(t, 1, block.ToolCount())
	var standalone []string
	for _, msg := range m.messages {
		if msg.Type == types.MessageTypeToolCall {
			standalone = append(standalone, msg.ToolCall.ID)
		}
	}
	assert.Equal(t, []string{"kept"}, standalone)
	block.SetExpanded(true)
	view := block.View()
	assert.NotContains(t, view, "in-block", "the retracted call no longer renders")
}
