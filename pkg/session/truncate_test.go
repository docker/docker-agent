package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/tools"
)

func TestGetMessages_PreservesAllToolContent(t *testing.T) {
	// Tool results and arguments are never modified by GetMessages.
	// Session compaction handles context limits instead.
	testAgent := agent.New("test-agent", "test instruction")

	largeArgs := `{"path":"big.go","content":"` + strings.Repeat("x", 50000) + `"}`
	largeResult := strings.Repeat("y", 50000)

	s := New()
	s.AddMessage(UserMessage("do something"))
	s.AddMessage(&Message{
		AgentName: "test-agent",
		Message: chat.Message{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{
					Name: "write_file", Arguments: largeArgs,
				}},
			},
		},
	})
	s.AddMessage(&Message{
		AgentName: "test-agent",
		Message: chat.Message{
			Role: chat.MessageRoleTool, ToolCallID: "call_1",
			Content: "file written successfully",
		},
	})
	s.AddMessage(UserMessage("now read it"))
	s.AddMessage(&Message{
		AgentName: "test-agent",
		Message: chat.Message{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "call_2", Type: "function", Function: tools.FunctionCall{
					Name: "read_file", Arguments: `{"path":"big.go"}`,
				}},
			},
		},
	})
	s.AddMessage(&Message{
		AgentName: "test-agent",
		Message: chat.Message{
			Role: chat.MessageRoleTool, ToolCallID: "call_2",
			Content: largeResult,
		},
	})

	messages := s.GetMessages(testAgent)

	// Verify tool results and arguments survive GetMessages verbatim.
	var foundCall1Args, foundCall2Args string
	var foundResult1, foundResult2 string
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleTool && msg.ToolCallID == "call_1" {
			foundResult1 = msg.Content
		}
		if msg.Role == chat.MessageRoleTool && msg.ToolCallID == "call_2" {
			foundResult2 = msg.Content
		}
		for _, tc := range msg.ToolCalls {
			switch tc.ID {
			case "call_1":
				foundCall1Args = tc.Function.Arguments
			case "call_2":
				foundCall2Args = tc.Function.Arguments
			}
		}
	}
	assert.Equal(t, "file written successfully", foundResult1)
	assert.Equal(t, largeResult, foundResult2)
	assert.Equal(t, largeArgs, foundCall1Args)
	assert.JSONEq(t, `{"path":"big.go"}`, foundCall2Args)
}

func TestGetMessages_PrefixStability(t *testing.T) {
	// Calling GetMessages multiple times must produce identical output.
	// This is critical for prompt caching: if the prefix changes between
	// calls, every subsequent API request is a cache miss.
	testAgent := agent.New("test-agent", "test instruction")

	s := New()
	s.AddMessage(UserMessage("hello"))
	s.AddMessage(&Message{
		AgentName: "test-agent",
		Message: chat.Message{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{
					Name: "read_file", Arguments: `{"path":"foo.go"}`,
				}},
			},
		},
	})
	s.AddMessage(&Message{
		AgentName: "test-agent",
		Message: chat.Message{
			Role: chat.MessageRoleTool, ToolCallID: "call_1",
			Content: "contents of foo.go",
		},
	})

	msgs1 := s.GetMessages(testAgent)
	msgs2 := s.GetMessages(testAgent)

	require.Len(t, msgs2, len(msgs1))
	for i := range msgs1 {
		assert.Equal(t, msgs1[i].Content, msgs2[i].Content,
			"message %d content should be identical between calls", i)
		assert.Equal(t, msgs1[i].Role, msgs2[i].Role,
			"message %d role should be identical between calls", i)
	}
}
