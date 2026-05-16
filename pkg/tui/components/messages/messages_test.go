package messages

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestViewDoesNotWrapWideLines(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(20, 5, sessionState).(*model)
	m.SetSize(20, 5)

	msg := types.Agent(types.MessageTypeAssistant, "", strings.Repeat("x", 200))
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))

	out := m.View()
	for line := range strings.SplitSeq(out, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 20)
	}
}

func TestLoadFromSessionIncludesReasoningContent(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{
				Message: chat.Message{
					Role:    chat.MessageRoleUser,
					Content: "Hello",
				},
			}),
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Let me think about this...",
					Content:          "Hello back!",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: user message + reasoning block + assistant content = 3 messages
	require.Len(t, m.messages, 3)
	// User message first
	assert.Equal(t, types.MessageTypeUser, m.messages[0].Type)
	assert.Equal(t, "Hello", m.messages[0].Content)
	// Reasoning block second (contains reasoning content)
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[1].Type)
	assert.Equal(t, "Let me think about this...", m.messages[1].Content)
	assert.Equal(t, "root", m.messages[1].Sender)
	// Verify the view is a reasoning block
	block, ok := m.views[1].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")
	assert.Equal(t, "Let me think about this...", block.Reasoning())
	// Assistant content third
	assert.Equal(t, types.MessageTypeAssistant, m.messages[2].Type)
	assert.Equal(t, "Hello back!", m.messages[2].Content)
	assert.Equal(t, "root", m.messages[2].Sender)
}

func TestLoadFromSessionReasoningOrderWithToolCalls(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "I should call a tool...",
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "test_tool", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "test_tool", Description: "A test tool"},
					},
					Content: "Tool result processed.",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: reasoning block (reasoning only) + assistant content + standalone tool call = 3 messages
	// The content breaks the reasoning block chain, so tool calls become standalone
	require.Len(t, m.messages, 3)
	// Reasoning block first (contains reasoning only, no tool calls)
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	assert.Equal(t, "I should call a tool...", m.messages[0].Content)
	// Verify the view is a reasoning block without tool calls
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")
	assert.Equal(t, "I should call a tool...", block.Reasoning())
	assert.Equal(t, 0, block.ToolCount(), "block should NOT contain tool calls (content broke the chain)")
	// Assistant content second
	assert.Equal(t, types.MessageTypeAssistant, m.messages[1].Type)
	assert.Equal(t, "Tool result processed.", m.messages[1].Content)
	// Tool call is standalone (after content broke the chain)
	assert.Equal(t, types.MessageTypeToolCall, m.messages[2].Type)
	assert.Equal(t, "test_tool", m.messages[2].ToolCall.Function.Name)
}

func TestLoadFromSessionReasoningOnlyNoContent(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Thinking deeply...",
					Content:          "", // No visible content, only reasoning
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: just the reasoning block (no assistant content)
	require.Len(t, m.messages, 1)
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	assert.Equal(t, "Thinking deeply...", m.messages[0].Content)
	// Verify the view is a reasoning block
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")
	assert.Equal(t, "Thinking deeply...", block.Reasoning())
}

func TestLoadFromSessionToolCallsOnlyNoReasoning(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "", // No reasoning
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "test_tool", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "test_tool", Description: "A test tool"},
					},
					Content: "Done.",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: assistant content + standalone tool call = 2 messages
	// Tool calls without reasoning should NOT go into a reasoning block
	require.Len(t, m.messages, 2)
	// Assistant content first (content is rendered before tool calls)
	assert.Equal(t, types.MessageTypeAssistant, m.messages[0].Type)
	assert.Equal(t, "Done.", m.messages[0].Content)
	// Tool call is standalone (not in a reasoning block)
	assert.Equal(t, types.MessageTypeToolCall, m.messages[1].Type)
	assert.Equal(t, "test_tool", m.messages[1].ToolCall.Function.Name)
}

func TestLoadFromSessionWithToolResults(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			// Assistant message with tool calls
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Let me check this...",
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path": "test.txt"}`}},
						{ID: "call-2", Function: tools.FunctionCall{Name: "list_dir", Arguments: `{"path": "."}`}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "read_file", Description: "Read a file"},
						{Name: "list_dir", Description: "List directory"},
					},
				},
			}),
			// Tool result for call-1
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-1",
					Content:    "File content here",
				},
			}),
			// Tool result for call-2
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-2",
					Content:    "file1.txt\nfile2.txt",
				},
			}),
			// Final assistant response
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:    chat.MessageRoleAssistant,
					Content: "I found the files.",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: reasoning block (reasoning + 2 tool calls with results) + assistant content = 2 messages
	require.Len(t, m.messages, 2)

	// First message should be reasoning block
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")
	assert.Equal(t, "Let me check this...", block.Reasoning())
	assert.Equal(t, 2, block.ToolCount(), "block should contain 2 tool calls")

	// Expand the block to see tool calls (completed tools are hidden when collapsed)
	block.SetExpanded(true)
	view := block.View()
	assert.Contains(t, view, "read_file", "expanded view should show read_file tool")
	assert.Contains(t, view, "list_dir", "expanded view should show list_dir tool")

	// Second message should be assistant content
	assert.Equal(t, types.MessageTypeAssistant, m.messages[1].Type)
	assert.Equal(t, "I found the files.", m.messages[1].Content)
}

func TestLoadFromSessionCombinesConsecutiveReasoningBlocks(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			// First assistant message with reasoning and tool call
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "First reasoning chunk.",
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "tool1", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "tool1", Description: "First tool"},
					},
				},
			}),
			// Tool result for call-1
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-1",
					Content:    "Result 1",
				},
			}),
			// Second assistant message with more reasoning and another tool call (consecutive, no content between)
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Second reasoning chunk.",
					ToolCalls: []tools.ToolCall{
						{ID: "call-2", Function: tools.FunctionCall{Name: "tool2", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "tool2", Description: "Second tool"},
					},
				},
			}),
			// Tool result for call-2
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-2",
					Content:    "Result 2",
				},
			}),
			// Third consecutive reasoning block
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Third reasoning chunk.",
					ToolCalls: []tools.ToolCall{
						{ID: "call-3", Function: tools.FunctionCall{Name: "tool3", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "tool3", Description: "Third tool"},
					},
				},
			}),
			// Tool result for call-3
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-3",
					Content:    "Result 3",
				},
			}),
			// Final assistant response (this breaks the chain)
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:    chat.MessageRoleAssistant,
					Content: "All done!",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Should have: 1 combined reasoning block + 1 assistant content = 2 messages
	require.Len(t, m.messages, 2, "consecutive reasoning blocks should be combined into one")

	// First message should be the combined reasoning block
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")

	// Block should contain all 3 tool calls
	assert.Equal(t, 3, block.ToolCount(), "combined block should contain all 3 tool calls")

	// Reasoning should contain all three chunks
	reasoning := block.Reasoning()
	assert.Contains(t, reasoning, "First reasoning chunk", "should contain first reasoning")
	assert.Contains(t, reasoning, "Second reasoning chunk", "should contain second reasoning")
	assert.Contains(t, reasoning, "Third reasoning chunk", "should contain third reasoning")

	// Expand to verify tools are present
	block.SetExpanded(true)
	view := block.View()
	assert.Contains(t, view, "tool1", "should contain tool1")
	assert.Contains(t, view, "tool2", "should contain tool2")
	assert.Contains(t, view, "tool3", "should contain tool3")

	// Second message should be assistant content
	assert.Equal(t, types.MessageTypeAssistant, m.messages[1].Type)
	assert.Equal(t, "All done!", m.messages[1].Content)
}

func TestLoadFromSessionStandaloneToolCallsWithResults(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			// Assistant message with tool calls only (no reasoning, no content)
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role: chat.MessageRoleAssistant,
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "test_tool", Arguments: `{"arg": "value"}`}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "test_tool", Description: "A test tool"},
					},
				},
			}),
			// Tool result
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-1",
					Content:    "Tool execution result",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: standalone tool call (not in reasoning block)
	require.Len(t, m.messages, 1)

	// Tool call should be standalone with result applied
	assert.Equal(t, types.MessageTypeToolCall, m.messages[0].Type)
	assert.Equal(t, "test_tool", m.messages[0].ToolCall.Function.Name)
	assert.Equal(t, "Tool execution result", m.messages[0].Content, "tool result should be applied to standalone tool call")
	assert.Equal(t, types.ToolStatusCompleted, m.messages[0].ToolStatus)
}

func TestLoadFromSessionToolCallsDuringReasoningNoContent(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			// Assistant message with reasoning and tool calls but NO content
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Let me think and use a tool...",
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "think_tool", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "think_tool", Description: "A thinking tool"},
					},
					// No Content - tool calls should stay in reasoning block
				},
			}),
			// Tool result
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-1",
					Content:    "Thought result",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	// Expect: reasoning block only (tool call inside it)
	require.Len(t, m.messages, 1)

	// Should be a reasoning block with the tool call inside
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")
	assert.Equal(t, "Let me think and use a tool...", block.Reasoning())
	assert.Equal(t, 1, block.ToolCount(), "tool call should be inside reasoning block when no content breaks the chain")
}

func TestLoadFromSessionReasoningWithContentToolResultsStandalone(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:             chat.MessageRoleAssistant,
					ReasoningContent: "Need to call a tool...",
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "test_tool", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "test_tool", Description: "A test tool"},
					},
					Content: "Done.",
				},
			}),
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-1",
					Content:    "Result\tvalue",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	require.Len(t, m.messages, 3)

	// Reasoning block should NOT contain tool calls
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok, "view should be a reasoning block")
	assert.Equal(t, 0, block.ToolCount(), "tool calls should be standalone after content breaks the chain")

	// Assistant content second
	assert.Equal(t, types.MessageTypeAssistant, m.messages[1].Type)
	assert.Equal(t, "Done.", m.messages[1].Content)

	// Tool call is standalone with result applied
	assert.Equal(t, types.MessageTypeToolCall, m.messages[2].Type)
	assert.Equal(t, "test_tool", m.messages[2].ToolCall.Function.Name)
	assert.Equal(t, "Result    value", m.messages[2].Content)
	assert.Equal(t, types.ToolStatusCompleted, m.messages[2].ToolStatus)
}

func TestLoadFromSessionMultipleStandaloneToolCallsWithContentAndResults(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	sess := &session.Session{
		ID: "test-session",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:    chat.MessageRoleAssistant,
					Content: "Here you go.",
					ToolCalls: []tools.ToolCall{
						{ID: "call-1", Function: tools.FunctionCall{Name: "tool1", Arguments: "{}"}},
						{ID: "call-2", Function: tools.FunctionCall{Name: "tool2", Arguments: "{}"}},
					},
					ToolDefinitions: []tools.Tool{
						{Name: "tool1", Description: "First tool"},
						{Name: "tool2", Description: "Second tool"},
					},
				},
			}),
			// Tool results (order shouldn't matter)
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-2",
					Content:    "Second\tresult",
				},
			}),
			session.NewMessageItem(&session.Message{
				AgentName: "root",
				Message: chat.Message{
					Role:       chat.MessageRoleTool,
					ToolCallID: "call-1",
					Content:    "First result",
				},
			}),
		},
	}

	m.LoadFromSession(sess)

	require.Len(t, m.messages, 3)

	// Assistant content first
	assert.Equal(t, types.MessageTypeAssistant, m.messages[0].Type)
	assert.Equal(t, "Here you go.", m.messages[0].Content)

	// Tool calls are standalone and in the original tool call order
	assert.Equal(t, types.MessageTypeToolCall, m.messages[1].Type)
	assert.Equal(t, "tool1", m.messages[1].ToolCall.Function.Name)
	assert.Equal(t, "First result", m.messages[1].Content)
	assert.Equal(t, types.ToolStatusCompleted, m.messages[1].ToolStatus)

	assert.Equal(t, types.MessageTypeToolCall, m.messages[2].Type)
	assert.Equal(t, "tool2", m.messages[2].ToolCall.Function.Name)
	assert.Equal(t, "Second    result", m.messages[2].Content)
	assert.Equal(t, types.ToolStatusCompleted, m.messages[2].ToolStatus)
}

// dynamicView is a stub layout.Model that changes its View output on Update
// and returns a non-nil command (simulating spinner tick behavior).
type dynamicView struct {
	frame int
}

func (d *dynamicView) Init() tea.Cmd { return nil }
func (d *dynamicView) Update(tea.Msg) (layout.Model, tea.Cmd) {
	d.frame++
	// Return a non-nil command to signal state change (like spinner.Tick)
	return d, func() tea.Msg { return nil }
}

func (d *dynamicView) View() string {
	return "frame-" + strconv.Itoa(d.frame)
}
func (d *dynamicView) SetSize(_, _ int) tea.Cmd { return nil }

func TestRenderCacheInvalidatesOnChildUpdate(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Insert a dynamic view that changes on each Update
	msg := types.Spinner()
	m.messages = append(m.messages, msg)
	m.views = append(m.views, &dynamicView{frame: 0})
	m.renderDirty = true

	// First render - should show frame-0
	view1 := m.View()
	assert.Contains(t, view1, "frame-0")

	// Update with any message - dynamic view will increment frame and return a cmd
	m.Update(struct{}{})

	// Second render - cache should be invalidated, showing frame-1
	view2 := m.View()
	assert.Contains(t, view2, "frame-1")
	assert.NotEqual(t, view1, view2, "View should change after Update with non-nil child cmd")
}

func TestRenderCacheInvalidatesOnAnimationTickWithAnimatedContent(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add a running tool call which has a spinner (animated content)
	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "running_tool", Arguments: `{}`},
	}, tools.Tool{Name: "running_tool", Description: "A running tool"}, types.ToolStatusRunning)
	m.messages = append(m.messages, toolMsg)
	m.views = append(m.views, m.createToolCallView(toolMsg))
	m.renderDirty = true

	// First render populates the cache.
	require.Contains(t, m.View(), "running_tool")
	m.renderDirty = false

	// An animation tick must refresh the cache so the spinner frame advances.
	// onAnimationTick now re-renders eagerly inside Update, so the resulting
	// View() output stays consistent with the latest tick.
	m.Update(animation.TickMsg{Frame: 1})

	require.NotEmpty(t, m.renderedLines)
	require.Contains(t, m.View(), "running_tool")
}

func TestRenderCacheNotInvalidatedOnAnimationTickWithoutAnimatedContent(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add a completed tool call (no spinner - not animated)
	toolMsg := types.ToolCallMessage("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "completed_tool", Arguments: `{}`},
	}, tools.Tool{Name: "completed_tool", Description: "A completed tool"}, types.ToolStatusCompleted)
	m.messages = append(m.messages, toolMsg)
	m.views = append(m.views, m.createToolCallView(toolMsg))
	m.renderDirty = true

	// First render
	view1 := m.View()
	require.Contains(t, view1, "completed_tool")

	// Clear the dirty flag to simulate cached state
	m.renderDirty = false

	// Send animation tick - should NOT invalidate cache because no animated content
	m.Update(animation.TickMsg{Frame: 1})

	// Cache should still be clean (not dirty)
	assert.False(t, m.renderDirty, "renderDirty should remain false after animation tick without animated content")
}

func TestHasAnimatedContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setupFunc    func(m *model)
		wantAnimated bool
	}{
		{
			name:         "empty model",
			setupFunc:    func(_ *model) {},
			wantAnimated: false,
		},
		{
			name: "spinner message",
			setupFunc: func(m *model) {
				msg := types.Spinner()
				m.messages = append(m.messages, msg)
				m.views = append(m.views, m.createMessageView(msg))
			},
			wantAnimated: true,
		},
		{
			name: "loading message",
			setupFunc: func(m *model) {
				msg := types.Loading("Loading...")
				m.messages = append(m.messages, msg)
				m.views = append(m.views, m.createMessageView(msg))
			},
			wantAnimated: true,
		},
		{
			name: "pending tool call",
			setupFunc: func(m *model) {
				toolMsg := types.ToolCallMessage("root", tools.ToolCall{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "pending_tool", Arguments: `{}`},
				}, tools.Tool{Name: "pending_tool"}, types.ToolStatusPending)
				m.messages = append(m.messages, toolMsg)
				m.views = append(m.views, m.createToolCallView(toolMsg))
			},
			wantAnimated: true,
		},
		{
			name: "running tool call",
			setupFunc: func(m *model) {
				toolMsg := types.ToolCallMessage("root", tools.ToolCall{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "running_tool", Arguments: `{}`},
				}, tools.Tool{Name: "running_tool"}, types.ToolStatusRunning)
				m.messages = append(m.messages, toolMsg)
				m.views = append(m.views, m.createToolCallView(toolMsg))
			},
			wantAnimated: true,
		},
		{
			name: "completed tool call",
			setupFunc: func(m *model) {
				toolMsg := types.ToolCallMessage("root", tools.ToolCall{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "completed_tool", Arguments: `{}`},
				}, tools.Tool{Name: "completed_tool"}, types.ToolStatusCompleted)
				m.messages = append(m.messages, toolMsg)
				m.views = append(m.views, m.createToolCallView(toolMsg))
			},
			wantAnimated: false,
		},
		{
			name: "assistant message",
			setupFunc: func(m *model) {
				msg := types.Agent(types.MessageTypeAssistant, "root", "Hello")
				m.messages = append(m.messages, msg)
				m.views = append(m.views, m.createMessageView(msg))
			},
			wantAnimated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionState := &service.SessionState{}
			m := NewScrollableView(80, 24, sessionState).(*model)
			m.SetSize(80, 24)
			tt.setupFunc(m)
			got := m.hasAnimatedContent()
			assert.Equal(t, tt.wantAnimated, got)
		})
	}
}

// BenchmarkMessagesView_RenderWhileScrolling benchmarks View() with scroll offset changes.
// This measures render cost only (no input handling or coalescing).
func BenchmarkMessagesView_RenderWhileScrolling(b *testing.B) {
	// Create a model with many messages to simulate a long conversation
	sessionState := &service.SessionState{}
	m := NewScrollableView(120, 40, sessionState).(*model)
	m.SetSize(120, 40)

	// Add 100 messages to create substantial history
	for range 100 {
		msg := types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("This is a test message with some content. ", 10))
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}

	// Initial render to populate cache
	m.View()

	b.ResetTimer()
	b.ReportAllocs()

	// Simulate scrolling by varying scroll offset
	for i := range b.N {
		// Vary scroll position to simulate wheel scrolling
		m.scrollOffset = (i % 50) * 2
		m.scrollview.SetScrollOffset(m.scrollOffset)
		_ = m.View()
	}
}

// BenchmarkMessagesView_LargeHistory benchmarks View() with a very large message history.
func BenchmarkMessagesView_LargeHistory(b *testing.B) {
	sessionState := &service.SessionState{}
	m := NewScrollableView(120, 40, sessionState).(*model)
	m.SetSize(120, 40)

	// Add 500 messages
	for i := range 500 {
		content := "Message " + strconv.Itoa(i) + ": " + strings.Repeat("content ", 20)
		msg := types.Agent(types.MessageTypeAssistant, "root", content)
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}

	// Initial render to populate cache
	m.View()

	b.ResetTimer()
	b.ReportAllocs()

	for i := range b.N {
		m.scrollOffset = (i % 100) * 5
		m.scrollview.SetScrollOffset(m.scrollOffset)
		_ = m.View()
	}
}

func TestIsSelectableMessage(t *testing.T) {
	t.Parallel()

	sessionPos := 1

	tests := []struct {
		name     string
		msg      *types.Message
		expected bool
	}{
		{
			name:     "assistant message is selectable",
			msg:      types.Agent(types.MessageTypeAssistant, "root", "Hello"),
			expected: true,
		},
		{
			name:     "reasoning block is selectable",
			msg:      types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "Thinking..."),
			expected: true,
		},
		{
			name: "user message with session position is selectable",
			msg: &types.Message{
				Type:            types.MessageTypeUser,
				Content:         "Hello",
				SessionPosition: &sessionPos,
			},
			expected: true,
		},
		{
			name: "user message without session position is not selectable",
			msg: &types.Message{
				Type:            types.MessageTypeUser,
				Content:         "Hello",
				SessionPosition: nil,
			},
			expected: false,
		},
		{
			name:     "tool call is not selectable",
			msg:      types.ToolCallMessage("root", tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "test", Arguments: "{}"}}, tools.Tool{Name: "test"}, types.ToolStatusCompleted),
			expected: false,
		},
		{
			name:     "spinner is not selectable",
			msg:      types.Spinner(),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionState := &service.SessionState{}
			m := NewScrollableView(80, 24, sessionState).(*model)
			m.SetSize(80, 24)

			m.messages = append(m.messages, tt.msg)
			m.views = append(m.views, m.createMessageView(tt.msg))

			got := m.isSelectableMessage(0)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestKeyEEmitsEditUserMessageMsg(t *testing.T) {
	t.Parallel()

	sessionPos := 1
	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add a user message with session position
	userMsg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "Hello world",
		SessionPosition: &sessionPos,
	}
	m.messages = append(m.messages, userMsg)
	m.views = append(m.views, m.createMessageView(userMsg))

	// Focus and select the message
	m.Focus()
	m.selectedMessageIndex = 0

	// Press 'e' key
	keyMsg := tea.KeyPressMsg{Code: 101} // 'e'
	_, cmd := m.Update(keyMsg)

	// Verify a command was returned
	require.NotNil(t, cmd, "expected a command to be returned")

	// Execute the command to get the message
	msg := cmd()
	editMsg, ok := msg.(tuimessages.EditUserMessageMsg)
	require.True(t, ok, "expected EditUserMessageMsg, got %T", msg)

	assert.Equal(t, 0, editMsg.MsgIndex)
	assert.Equal(t, 1, editMsg.SessionPosition)
	assert.Equal(t, "Hello world", editMsg.OriginalContent)
}

func TestKeyENoOpForNonEditableUserMessage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add a user message WITHOUT session position (not editable)
	userMsg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "Hello world",
		SessionPosition: nil,
	}
	m.messages = append(m.messages, userMsg)
	m.views = append(m.views, m.createMessageView(userMsg))

	// Focus and select the message
	m.Focus()
	m.selectedMessageIndex = 0

	// Press 'e' key
	keyMsg := tea.KeyPressMsg{Code: 101} // 'e'
	_, cmd := m.Update(keyMsg)

	// Should return nil command since message is not editable
	assert.Nil(t, cmd, "expected no command for non-editable user message")
}

func TestKeyENoOpForAssistantMessage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add an assistant message
	assistantMsg := types.Agent(types.MessageTypeAssistant, "root", "Hello")
	m.messages = append(m.messages, assistantMsg)
	m.views = append(m.views, m.createMessageView(assistantMsg))

	// Focus and select the message
	m.Focus()
	m.selectedMessageIndex = 0

	// Press 'e' key
	keyMsg := tea.KeyPressMsg{Code: 101} // 'e'
	_, cmd := m.Update(keyMsg)

	// Should return nil command since it's an assistant message
	assert.Nil(t, cmd, "expected no command for assistant message")
}

func TestUserMessageNavigationWithArrowKeys(t *testing.T) {
	t.Parallel()

	sessionPos1 := 1
	sessionPos2 := 2

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add messages: user (editable), assistant, user (editable)
	userMsg1 := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "First user message",
		SessionPosition: &sessionPos1,
	}
	assistantMsg := types.Agent(types.MessageTypeAssistant, "root", "Response")
	userMsg2 := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "Second user message",
		SessionPosition: &sessionPos2,
	}

	m.messages = append(m.messages, userMsg1, assistantMsg, userMsg2)
	for _, msg := range m.messages {
		m.views = append(m.views, m.createMessageView(msg))
	}

	// Focus the component - this auto-selects the last assistant message
	m.Focus()

	// Focus() selects the last assistant message (index 1)
	assert.Equal(t, 1, m.selectedMessageIndex, "Focus should select last assistant message")

	// Press up to go back to first user message
	upMsg := tea.KeyPressMsg{Code: tea.KeyUp}
	m.Update(upMsg)
	assert.Equal(t, 0, m.selectedMessageIndex, "should select first user message after up")

	// Press down to go back to assistant message
	downMsg := tea.KeyPressMsg{Code: tea.KeyDown}
	m.Update(downMsg)
	assert.Equal(t, 1, m.selectedMessageIndex, "should select assistant message after down")

	// Press down to go to second user message
	m.Update(downMsg)
	assert.Equal(t, 2, m.selectedMessageIndex, "should select second user message after down")
}

func TestBindingsIncludesEditKeyWhenUserMessageSelected(t *testing.T) {
	t.Parallel()

	sessionPos := 1
	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add a user message with session position
	userMsg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "Hello world",
		SessionPosition: &sessionPos,
	}
	m.messages = append(m.messages, userMsg)
	m.views = append(m.views, m.createMessageView(userMsg))

	// Focus and select the user message
	m.Focus()

	bindings := m.Bindings()

	// Find the 'e' binding - should be present when user message is selected
	var foundE bool
	for _, b := range bindings {
		if slices.Contains(b.Keys(), "e") {
			foundE = true
		}
	}
	assert.True(t, foundE, "Bindings should include 'e' key when user message is selected")
}

func TestAddOrUpdateToolCallFindsToolInNonActiveReasoningBlock(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	agentName := "root"
	toolCall := tools.ToolCall{
		ID:       "call_1",
		Function: tools.FunctionCall{Name: "go_workspace", Arguments: `{}`},
	}
	toolDef := tools.Tool{Name: "go_workspace"}

	// Step 1: Add a reasoning block and a tool call inside it (simulates PartialToolCallEvent)
	m.AppendReasoning(agentName, "Thinking...")
	require.Len(t, m.messages, 1)
	assert.Equal(t, types.MessageTypeAssistantReasoningBlock, m.messages[0].Type)

	m.AddOrUpdateToolCall(agentName, toolCall, toolDef, types.ToolStatusPending)
	block, ok := m.views[0].(*reasoningblock.Model)
	require.True(t, ok)
	require.True(t, block.HasToolCall("call_1"))

	// Step 2: Append an assistant message so the reasoning block is no longer the last message
	m.AppendToLastMessage(agentName, "Here is the answer.")
	require.Len(t, m.messages, 2)
	assert.Equal(t, types.MessageTypeAssistant, m.messages[1].Type)

	// Step 3: Update the tool call to Running (simulates ToolCallEvent)
	// Before the fix, this would not find the tool in the old reasoning block
	// and would create a duplicate standalone entry.
	m.AddOrUpdateToolCall(agentName, toolCall, toolDef, types.ToolStatusRunning)

	// Verify: still only 2 messages (no duplicate tool call created)
	assert.Len(t, m.messages, 2, "should not create a duplicate tool call message")

	// Verify the tool call in the reasoning block was updated (not duplicated)
	block, ok = m.views[0].(*reasoningblock.Model)
	require.True(t, ok)
	assert.True(t, block.HasToolCall("call_1"))
	assert.Equal(t, 1, block.ToolCount(), "reasoning block should still have exactly one tool call")
}

func TestBindingsExcludesEditKeyWhenAssistantMessageSelected(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add an assistant message
	assistantMsg := types.Agent(types.MessageTypeAssistant, "root", "Hello")
	m.messages = append(m.messages, assistantMsg)
	m.views = append(m.views, m.createMessageView(assistantMsg))

	// Focus and select the assistant message
	m.Focus()

	bindings := m.Bindings()

	// Find the 'e' binding - should NOT be present when assistant message is selected
	var foundE bool
	for _, b := range bindings {
		if slices.Contains(b.Keys(), "e") {
			foundE = true
		}
	}
	assert.False(t, foundE, "Bindings should NOT include 'e' key when assistant message is selected")
}

func TestKeyGAndShiftGScrollMessagesView(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 10, sessionState).(*model)
	m.SetSize(80, 10)

	// Add enough messages to require scrolling.
	for i := range 20 {
		content := "Message " + strconv.Itoa(i) + ": " + strings.Repeat("line\n", 5)
		msg := types.Agent(types.MessageTypeAssistant, "root", content)
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}

	// Select the messages view.
	m.Focus()

	// Render once to compute layout (auto-scrolls to the bottom).
	m.View()
	require.Positive(t, m.scrollOffset, "precondition: should not start at the top")

	// 'g' jumps to the very top of the view.
	m.Update(tea.KeyPressMsg{Code: 'g'})
	assert.Equal(t, 0, m.scrollOffset, "g should scroll to the top")

	// 'G' jumps back to the very bottom of the view.
	m.Update(tea.KeyPressMsg{Code: 'G'})
	m.View() // apply scroll clamp
	wantOffset := max(0, m.totalScrollableHeight()-m.height)
	assert.Equal(t, wantOffset, m.scrollOffset, "G should scroll to the bottom")
}

func TestKeyGAndGWithEmptyMessages(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 10, sessionState).(*model)
	m.SetSize(80, 10)

	// No messages - should not panic
	m.Update(tea.KeyPressMsg{Code: 'g'})
	assert.Equal(t, 0, m.scrollOffset, "g with empty messages should set offset to 0")

	m.Update(tea.KeyPressMsg{Code: 'G'})
	assert.Equal(t, 0, m.scrollOffset, "G with empty messages should set offset to 0")
}

func TestKeyGAndGDuringInlineEdit(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 10, sessionState).(*model)
	m.SetSize(80, 10)

	sessionPos := 0
	userMsg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "test",
		SessionPosition: &sessionPos,
	}
	m.messages = append(m.messages, userMsg)
	m.views = append(m.views, m.createMessageView(userMsg))

	// Start inline edit
	m.StartInlineEdit(0, 0, "test")
	require.Equal(t, 0, m.inlineEditMsgIndex, "should be in inline edit mode")

	initialValue := m.inlineEditTextarea.Value()
	initialOffset := m.scrollOffset

	// 'g' should be forwarded to textarea, not trigger scroll
	m.Update(tea.KeyPressMsg(tea.Key{Code: 'g', Text: "g"}))
	assert.Contains(t, m.inlineEditTextarea.Value(), "g", "g should be typed into textarea during inline edit")
	assert.NotEqual(t, initialValue, m.inlineEditTextarea.Value(), "textarea value should change")

	// Scroll offset should not change
	assert.Equal(t, initialOffset, m.scrollOffset, "scroll offset should not change during inline edit")

	// 'G' should also be forwarded to textarea
	m.Update(tea.KeyPressMsg(tea.Key{Code: 'G', Text: "G"}))
	assert.Contains(t, m.inlineEditTextarea.Value(), "G", "G should be typed into textarea during inline edit")
	assert.Equal(t, initialOffset, m.scrollOffset, "scroll offset should not change during inline edit")
}
