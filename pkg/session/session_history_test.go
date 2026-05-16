package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionNumHistoryItems(t *testing.T) {
	tests := []struct {
		name                     string
		numHistoryItems          int
		messageCount             int
		expectedConversationMsgs int
	}{
		{
			name:            "limit to 3 conversation messages — user messages protected",
			numHistoryItems: 3,
			messageCount:    10,
			// 10 user (all protected) + 10 assistant. Need to remove 17, but only 10 removable.
			// Result: 10 users + 0 assistants = 10
			expectedConversationMsgs: 10,
		},
		{
			name:            "limit to 5 conversation messages — user messages protected",
			numHistoryItems: 5,
			messageCount:    8,
			// 8 user (all protected) + 8 assistant. Need to remove 11, but only 8 removable.
			// Result: 8 users + 0 assistants = 8
			expectedConversationMsgs: 8,
		},
		{
			name:                     "fewer messages than limit",
			numHistoryItems:          10,
			messageCount:             5,
			expectedConversationMsgs: 10, // 5 user + 5 assistant = 10 total conversation messages
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testAgent := agent.New("test-agent", "test instruction",
				agent.WithNumHistoryItems(tt.numHistoryItems))

			s := New()
			for i := range tt.messageCount {
				s.AddMessage(UserMessage(fmt.Sprintf("Message %d", i)))
				s.AddMessage(&Message{
					AgentName: "test-agent",
					Message: chat.Message{
						Role:    chat.MessageRoleAssistant,
						Content: fmt.Sprintf("Response %d", i),
					},
				})
			}

			messages := s.GetMessages(testAgent)

			// Count conversation messages (non-system)
			conversationCount := 0
			systemCount := 0
			for _, msg := range messages {
				if msg.Role == chat.MessageRoleSystem {
					systemCount++
				} else {
					conversationCount++
				}
			}

			// System messages should always be present (at least the instruction)
			assert.Positive(t, systemCount, "Should have system messages")

			assert.Equal(t, tt.expectedConversationMsgs, conversationCount,
				"Conversation messages should match expected count")
		})
	}
}

func TestTrimMessagesPreservesSystemMessages(t *testing.T) {
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "System instruction 1"},
		{Role: chat.MessageRoleSystem, Content: "System instruction 2"},
		{Role: chat.MessageRoleUser, Content: "User message 1"},
		{Role: chat.MessageRoleAssistant, Content: "Assistant response 1"},
		{Role: chat.MessageRoleSystem, Content: "Tool instruction"},
		{Role: chat.MessageRoleUser, Content: "User message 2"},
		{Role: chat.MessageRoleAssistant, Content: "Assistant response 2"},
		{Role: chat.MessageRoleUser, Content: "User message 3"},
		{Role: chat.MessageRoleAssistant, Content: "Assistant response 3"},
	}

	trimmed := trimMessages(messages, 1)

	// Count message types
	systemCount := 0
	userCount := 0
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleSystem {
			systemCount++
		}
		if msg.Role == chat.MessageRoleUser {
			userCount++
		}
	}

	// All system messages should be preserved
	assert.Equal(t, 3, systemCount, "All system messages should be preserved")
	// All user messages should be preserved even with maxItems=1
	assert.Equal(t, 3, userCount, "All user messages should be preserved")
}

func TestTrimMessagesConversationLimit(t *testing.T) {
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "System prompt"},
		{Role: chat.MessageRoleUser, Content: "Message 1"},
		{Role: chat.MessageRoleAssistant, Content: "Response 1"},
		{Role: chat.MessageRoleUser, Content: "Message 2"},
		{Role: chat.MessageRoleAssistant, Content: "Response 2"},
		{Role: chat.MessageRoleUser, Content: "Message 3"},
		{Role: chat.MessageRoleAssistant, Content: "Response 3"},
		{Role: chat.MessageRoleUser, Content: "Message 4"},
		{Role: chat.MessageRoleAssistant, Content: "Response 4"},
	}

	// 8 conversation messages: 4 user + 4 assistant
	// User messages are always protected, so only assistant messages can be trimmed.
	testCases := []struct {
		limit                int
		expectedSystem       int
		expectedUser         int
		expectedConversation int // total non-system
	}{
		// limit=2: need to remove 6 of 8, but 4 are protected users → only 4 assistants removable → remove 4
		{limit: 2, expectedSystem: 1, expectedUser: 4, expectedConversation: 4},
		// limit=4: need to remove 4 of 8, 4 are protected → remove all 4 assistants
		{limit: 4, expectedSystem: 1, expectedUser: 4, expectedConversation: 4},
		// limit=8: no trimming needed (8 <= 8)
		{limit: 8, expectedSystem: 1, expectedUser: 4, expectedConversation: 8},
		// limit=100: no trimming needed
		{limit: 100, expectedSystem: 1, expectedUser: 4, expectedConversation: 8},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("limit_%d", tc.limit), func(t *testing.T) {
			trimmed := trimMessages(messages, tc.limit)

			systemCount := 0
			userCount := 0
			conversationCount := 0
			for _, msg := range trimmed {
				switch msg.Role {
				case chat.MessageRoleSystem:
					systemCount++
				case chat.MessageRoleUser:
					userCount++
					conversationCount++
				default:
					conversationCount++
				}
			}

			assert.Equal(t, tc.expectedSystem, systemCount, "System message count")
			assert.Equal(t, tc.expectedUser, userCount, "User messages should always be preserved")
			assert.Equal(t, tc.expectedConversation, conversationCount, "Conversation message count")
		})
	}
}

func TestTrimMessagesWithToolCallsPreservation(t *testing.T) {
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "System prompt"},
		{Role: chat.MessageRoleUser, Content: "Old message"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "Old response with tool",
			ToolCalls: []tools.ToolCall{
				{ID: "old_tool_1", Function: tools.FunctionCall{Name: "test"}},
			},
		},
		{
			Role:       chat.MessageRoleTool,
			Content:    "Old tool result",
			ToolCallID: "old_tool_1",
		},
		{Role: chat.MessageRoleUser, Content: "Recent message"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "Recent response with tool",
			ToolCalls: []tools.ToolCall{
				{ID: "recent_tool_1", Function: tools.FunctionCall{Name: "test"}},
			},
		},
		{
			Role:       chat.MessageRoleTool,
			Content:    "Recent tool result",
			ToolCallID: "recent_tool_1",
		},
	}

	// Limit to 3 conversation messages
	trimmed := trimMessages(messages, 3)

	toolCallIDs := make(map[string]bool)
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleAssistant {
			for _, tc := range msg.ToolCalls {
				toolCallIDs[tc.ID] = true
			}
		}
	}

	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleTool {
			assert.True(t, toolCallIDs[msg.ToolCallID],
				"Tool result should have a corresponding tool call")
		}
	}

	// Both user messages should be preserved
	userMessages := 0
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleUser {
			userMessages++
		}
	}
	assert.Equal(t, 2, userMessages, "Both user messages should be preserved")
}

func TestTrimMessagesPreservesUserMessagesInAgenticLoop(t *testing.T) {
	// Simulate a single-turn agentic loop: one user message followed by many tool calls
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "System prompt"},
		{Role: chat.MessageRoleUser, Content: "Analyze MR #123 and build an integration plan"},
	}

	for i := range 30 {
		toolID := fmt.Sprintf("tool_%d", i)
		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: fmt.Sprintf("Calling tool %d", i),
			ToolCalls: []tools.ToolCall{
				{ID: toolID, Function: tools.FunctionCall{Name: "shell"}},
			},
		}, chat.Message{
			Role:       chat.MessageRoleTool,
			Content:    fmt.Sprintf("Tool result %d", i),
			ToolCallID: toolID,
		})
	}

	// 61 conversation messages (1 user + 30 assistant + 30 tool), limit to 30
	trimmed := trimMessages(messages, 30)

	// The user message must survive
	var userMessages []string
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleUser {
			userMessages = append(userMessages, msg.Content)
		}
	}

	assert.Len(t, userMessages, 1, "User message must be preserved")
	assert.Equal(t, "Analyze MR #123 and build an integration plan", userMessages[0])

	// Tool call consistency: every tool result must have a matching assistant tool call
	toolCallIDs := make(map[string]bool)
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleAssistant {
			for _, tc := range msg.ToolCalls {
				toolCallIDs[tc.ID] = true
			}
		}
	}
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleTool {
			assert.True(t, toolCallIDs[msg.ToolCallID],
				"Tool result %s should have a corresponding assistant tool call", msg.ToolCallID)
		}
	}
}

func TestTrimMessagesPreservesAllUserMessages(t *testing.T) {
	// Multiple user messages interspersed with tool calls
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "System prompt"},
		{Role: chat.MessageRoleUser, Content: "First request"},
	}

	for i := range 10 {
		toolID := fmt.Sprintf("tool_%d", i)
		messages = append(messages, chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{{ID: toolID}},
		}, chat.Message{
			Role:       chat.MessageRoleTool,
			Content:    fmt.Sprintf("result %d", i),
			ToolCallID: toolID,
		})
	}

	messages = append(messages, chat.Message{Role: chat.MessageRoleUser, Content: "Follow-up request"})

	for i := 10; i < 20; i++ {
		toolID := fmt.Sprintf("tool_%d", i)
		messages = append(messages, chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{{ID: toolID}},
		}, chat.Message{
			Role:       chat.MessageRoleTool,
			Content:    fmt.Sprintf("result %d", i),
			ToolCallID: toolID,
		})
	}

	// 42 conversation messages (2 user + 20 assistant + 20 tool), limit to 10
	trimmed := trimMessages(messages, 10)

	var userContents []string
	for _, msg := range trimmed {
		if msg.Role == chat.MessageRoleUser {
			userContents = append(userContents, msg.Content)
		}
	}

	assert.Len(t, userContents, 2, "Both user messages must be preserved")
	assert.Equal(t, "First request", userContents[0])
	assert.Equal(t, "Follow-up request", userContents[1])
}
