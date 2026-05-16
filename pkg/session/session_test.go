package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestTrimMessagesWithToolCalls(t *testing.T) {
	messages := []chat.Message{
		{
			Role:    chat.MessageRoleUser,
			Content: "first message",
		},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "response with tool",
			ToolCalls: []tools.ToolCall{
				{
					ID: "tool1",
				},
			},
		},
		{
			Role:       chat.MessageRoleTool,
			Content:    "tool result",
			ToolCallID: "tool1",
		},
		{
			Role:    chat.MessageRoleUser,
			Content: "second message",
		},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "another response",
			ToolCalls: []tools.ToolCall{
				{
					ID: "tool2",
				},
			},
		},
		{
			Role:       chat.MessageRoleTool,
			Content:    "tool result 2",
			ToolCallID: "tool2",
		},
	}

	// Use 3 as the limit to force trimming
	maxItems := 3

	result := trimMessages(messages, maxItems)

	// Both user messages are protected, so result includes them plus the most recent assistant/tool pair
	toolCalls := make(map[string]bool)
	for _, msg := range result {
		if msg.Role == chat.MessageRoleAssistant {
			for _, tool := range msg.ToolCalls {
				toolCalls[tool.ID] = true
			}
		}
		if msg.Role == chat.MessageRoleTool {
			assert.True(t, toolCalls[msg.ToolCallID], "tool result should have corresponding assistant message")
		}
	}
}

func TestGetMessagesWithToolCalls(t *testing.T) {
	testAgent := &agent.Agent{}

	s := New()

	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleUser,
		Content: "test message",
	}))

	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "using tool",
		ToolCalls: []tools.ToolCall{
			{
				ID: "test-tool",
			},
		},
	}))

	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "test-tool",
		Content:    "tool result",
	}))

	messages := s.GetMessages(testAgent)

	toolCalls := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleAssistant {
			for _, tool := range msg.ToolCalls {
				toolCalls[tool.ID] = true
			}
		}
		if msg.Role == chat.MessageRoleTool {
			assert.True(t, toolCalls[msg.ToolCallID], "tool result should have corresponding assistant message")
		}
	}
}

func TestGetMessagesWithSummary(t *testing.T) {
	testAgent := &agent.Agent{}

	s := New()

	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleUser,
		Content: "first message",
	}))
	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "first response",
	}))

	s.Messages = append(s.Messages, Item{Summary: "This is a summary of the conversation so far"})

	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleUser,
		Content: "message after summary",
	}))
	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "response after summary",
	}))

	messages := s.GetMessages(testAgent)

	// Count non-system messages (user and assistant only)
	userAssistantMessages := 0
	summaryFound := false
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleUser || msg.Role == chat.MessageRoleAssistant {
			userAssistantMessages++
		}
		if msg.Role == chat.MessageRoleUser && msg.Content == "Session Summary: This is a summary of the conversation so far" {
			summaryFound = true
		}
	}

	// We should have:
	// - 1 summary user message
	// - 2 messages after the summary (user + assistant)
	// - Various other system messages from agent setup
	assert.True(t, summaryFound, "should include summary as user message")
	assert.Equal(t, 3, userAssistantMessages, "should only include messages after summary")
}

func TestGetMessages_Instructions(t *testing.T) {
	testAgent := agent.New("root", "instructions")

	s := New()
	messages := s.GetMessages(testAgent)

	assert.Len(t, messages, 1)
	assert.Equal(t, "instructions", messages[0].Content)
	assert.True(t, messages[0].CacheControl)
}

func TestGetMessages_CacheControl(t *testing.T) {
	testAgent := agent.New("root", "instructions", agent.WithToolSets(&todo.Tool{}))

	s := New()
	messages := s.GetMessages(testAgent)

	assert.Len(t, messages, 2)
	assert.Equal(t, "instructions", messages[0].Content)
	assert.False(t, messages[0].CacheControl)

	assert.Contains(t, messages[1].Content, "Todo Tools")
	assert.True(t, messages[1].CacheControl)
}

func TestGetMessages_CacheControlWithSummary(t *testing.T) {
	// Caching contract pinned by this test:
	//
	//   - The last invariant system message gets a cache-control marker.
	//   - The last caller-supplied extra (typically turn_start hook output)
	//     ALSO gets a cache-control marker so stable per-session/per-day
	//     extras (AddPromptFiles, AddEnvironmentInfo) participate in
	//     prompt caching. This matches the prior
	//     buildContextSpecificSystemMessages caching behavior.
	//   - Summary and conversation messages are not cache-controlled.
	testAgent := agent.New("root", "instructions",
		agent.WithToolSets(&todo.Tool{}),
	)

	s := New()
	s.Messages = append(s.Messages, Item{Summary: "Test summary"})

	extra := chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: "Today's date: 2026-04-25",
	}
	messages := s.GetMessages(testAgent, extra)

	var checkpointIndices []int
	for i, msg := range messages {
		if msg.Role == chat.MessageRoleSystem && msg.CacheControl {
			checkpointIndices = append(checkpointIndices, i)
		}
	}

	require.Len(t, checkpointIndices, 2,
		"invariant and last-extra messages should each be cache-controlled")

	// Checkpoint #1: last invariant message (toolset instructions).
	assert.Contains(t, messages[checkpointIndices[0]].Content, "Todo Tools",
		"checkpoint #1 must land on the last invariant message")

	// Checkpoint #2: last extra (the date system message).
	assert.Equal(t, extra.Content, messages[checkpointIndices[1]].Content,
		"checkpoint #2 must land on the last extra system message")

	// The extra must come AFTER the invariant block.
	assert.Greater(t, checkpointIndices[1], checkpointIndices[0],
		"extras must come AFTER the invariant cache checkpoint")
}

func TestGetLastUserMessages(t *testing.T) {
	t.Parallel()

	t.Run("empty session returns empty slice", func(t *testing.T) {
		t.Parallel()
		s := New()
		assert.Empty(t, s.GetLastUserMessages(2))
	})

	t.Run("session with fewer messages than requested returns all", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "Only message",
		}))
		msgs := s.GetLastUserMessages(2)
		assert.Len(t, msgs, 1)
		assert.Equal(t, "Only message", msgs[0])
	})

	t.Run("session returns last n user messages in order", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "First",
		}))
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Response 1",
		}))
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "Second",
		}))
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Response 2",
		}))
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "Third",
		}))

		msgs := s.GetLastUserMessages(2)
		assert.Len(t, msgs, 2)
		assert.Equal(t, "Second", msgs[0]) // Ordered oldest to newest
		assert.Equal(t, "Third", msgs[1])
	})

	t.Run("skips empty user messages", func(t *testing.T) {
		t.Parallel()
		s := New()
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "First",
		}))
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "   ", // Empty after trim
		}))
		s.AddMessage(NewAgentMessage("", &chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "Third",
		}))

		msgs := s.GetLastUserMessages(2)
		assert.Len(t, msgs, 2)
		assert.Equal(t, "First", msgs[0])
		assert.Equal(t, "Third", msgs[1])
	})
}

func TestEvalCriteriaUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    EvalCriteria
		wantErr bool
	}{
		{
			name:  "valid fields",
			input: `{"relevance":["is correct"],"size":"M","setup":"echo hello","working_dir":"mydir"}`,
			want: EvalCriteria{
				Relevance:  []string{"is correct"},
				Size:       "M",
				Setup:      "echo hello",
				WorkingDir: "mydir",
			},
		},
		{
			name:  "empty object",
			input: `{}`,
			want:  EvalCriteria{},
		},
		{
			name:    "unknown field rejected",
			input:   `{"relevance":[],"unknown_field":"value"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got EvalCriteria
			err := json.Unmarshal([]byte(tt.input), &got)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSanitizeToolCalls(t *testing.T) {
	t.Parallel()

	t.Run("no-op when all tool calls have results", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hi"},
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "tc1", Function: tools.FunctionCall{Name: "shell"}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "tc1", Content: "ok"},
			{Role: chat.MessageRoleAssistant, Content: "done"},
		}
		result := sanitizeToolCalls(messages)
		assert.Equal(t, messages, result)
	})

	t.Run("injects synthetic result for missing tool result", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hi"},
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "tc1", Function: tools.FunctionCall{Name: "shell"}},
				},
			},
		}
		result := sanitizeToolCalls(messages)

		require.Len(t, result, 3)
		assert.Equal(t, chat.MessageRoleTool, result[2].Role)
		assert.Equal(t, "tc1", result[2].ToolCallID)
		assert.True(t, result[2].IsError)
		assert.Equal(t, "No result provided", result[2].Content)
	})

	t.Run("handles multiple tool calls with partial results", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hi"},
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "tc1", Function: tools.FunctionCall{Name: "read_file"}},
					{ID: "tc2", Function: tools.FunctionCall{Name: "write_file"}},
					{ID: "tc3", Function: tools.FunctionCall{Name: "shell"}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "tc1", Content: "file contents"},
			// tc2 and tc3 are missing
		}
		result := sanitizeToolCalls(messages)

		// Original 3 messages + 2 synthetic results
		require.Len(t, result, 5)

		// assistant, then existing tc1 result, then synthetics for tc2/tc3 flushed at end
		assert.Equal(t, chat.MessageRoleAssistant, result[1].Role)
		assert.Equal(t, "tc1", result[2].ToolCallID)
		assert.False(t, result[2].IsError)
		assert.Equal(t, "tc2", result[3].ToolCallID)
		assert.True(t, result[3].IsError)
		assert.Equal(t, "tc3", result[4].ToolCallID)
		assert.True(t, result[4].IsError)
	})

	t.Run("no tool calls at all is a no-op", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
			{Role: chat.MessageRoleAssistant, Content: "hi there"},
		}
		result := sanitizeToolCalls(messages)
		assert.Equal(t, messages, result)
	})

	t.Run("multiple assistant messages with missing results", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hi"},
			{
				Role:      chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{{ID: "tc1"}},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "tc1", Content: "ok"},
			{
				Role:      chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{{ID: "tc2"}},
			},
			// tc2 result missing (crash)
		}
		result := sanitizeToolCalls(messages)

		require.Len(t, result, 5)
		assert.Equal(t, "tc2", result[4].ToolCallID)
		assert.True(t, result[4].IsError)
	})

	t.Run("flushes synthetics before next user message", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hi"},
			{
				Role:      chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{{ID: "tc1", Function: tools.FunctionCall{Name: "shell"}}},
			},
			// no tool result — user responds before result arrives
			{Role: chat.MessageRoleUser, Content: "never mind"},
			{Role: chat.MessageRoleAssistant, Content: "ok"},
		}
		result := sanitizeToolCalls(messages)

		// synthetic tc1 result should be injected before the second user message
		require.Len(t, result, 5)
		assert.Equal(t, chat.MessageRoleAssistant, result[1].Role)
		assert.Equal(t, "tc1", result[2].ToolCallID)
		assert.True(t, result[2].IsError)
		assert.Equal(t, chat.MessageRoleUser, result[3].Role)
		assert.Equal(t, chat.MessageRoleAssistant, result[4].Role)
	})

	t.Run("flushes synthetics before next assistant with tool calls", func(t *testing.T) {
		t.Parallel()
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hi"},
			{
				Role:      chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{{ID: "tc1"}},
			},
			// no result for tc1, model immediately issues another tool call
			{
				Role:      chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{{ID: "tc2"}},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "tc2", Content: "ok"},
		}
		result := sanitizeToolCalls(messages)

		require.Len(t, result, 5)
		// synthetic for tc1 inserted before the second assistant message
		assert.Equal(t, "tc1", result[2].ToolCallID)
		assert.True(t, result[2].IsError)
		assert.Len(t, result[3].ToolCalls, 1)
		assert.Equal(t, "tc2", result[3].ToolCalls[0].ID)
		assert.Equal(t, "tc2", result[4].ToolCallID)
		assert.False(t, result[4].IsError)
	})

	t.Run("empty messages returns empty", func(t *testing.T) {
		t.Parallel()
		result := sanitizeToolCalls(nil)
		assert.Nil(t, result)

		result = sanitizeToolCalls([]chat.Message{})
		assert.Empty(t, result)
	})
}

func TestGetMessages_SanitizesOrphanedToolCalls(t *testing.T) {
	testAgent := &agent.Agent{}

	s := New()
	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role:    chat.MessageRoleUser,
		Content: "do something",
	}))
	s.AddMessage(NewAgentMessage("", &chat.Message{
		Role: chat.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{
			{ID: "orphan1", Function: tools.FunctionCall{Name: "shell"}},
			{ID: "orphan2", Function: tools.FunctionCall{Name: "read_file"}},
		},
	}))
	// No tool result messages — simulating a crash mid-run

	messages := s.GetMessages(testAgent)

	// Verify every tool call ID has a matching tool result
	callIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			callIDs[tc.ID] = true
		}
		if msg.Role == chat.MessageRoleTool {
			resultIDs[msg.ToolCallID] = true
		}
	}
	for id := range callIDs {
		assert.True(t, resultIDs[id], "tool call %s should have a matching result", id)
	}
}

func TestTransferTaskPromptExcludesParents(t *testing.T) {
	t.Parallel()

	// Build hierarchy: planner -> root -> librarian
	// root's sub-agents: [librarian]
	// root's parents: [planner] (set by planner listing root as a sub-agent)
	librarian := agent.New("librarian", "", agent.WithDescription("Library agent"))
	root := agent.New("root", "You are the root agent",
		agent.WithDescription("Root agent"),
	)
	planner := agent.New("planner", "",
		agent.WithDescription("Planner agent"),
	)
	// Connect: root -> librarian (root has librarian as sub-agent)
	agent.WithSubAgents(librarian)(root)
	// Connect: planner -> root (planner has root as sub-agent, making root's parent = planner)
	agent.WithSubAgents(root)(planner)

	// Verify parent relationship was established
	require.Len(t, root.Parents(), 1)
	assert.Equal(t, "planner", root.Parents()[0].Name())

	s := New()
	messages := s.GetMessages(root)

	// Find the system message about sub-agents
	var subAgentMsg string
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleSystem && strings.Contains(msg.Content, "transfer_task") {
			subAgentMsg = msg.Content
			break
		}
	}

	require.NotEmpty(t, subAgentMsg, "should have a sub-agent system message")
	assert.Contains(t, subAgentMsg, "librarian", "should list librarian as a valid sub-agent")
	assert.NotContains(t, subAgentMsg, "planner", "should NOT list parent agent planner as a valid transfer target")
}

func TestNormalizeMessageContent(t *testing.T) {
	t.Parallel()

	img := chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,AAAA"}}

	tests := []struct {
		name  string
		input []chat.Message
		want  []chat.Message
	}{
		{
			name:  "empty input",
			input: nil,
			want:  nil,
		},
		{
			name: "whitespace-only user message dropped",
			input: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "   \n\t  "},
			},
			want: nil,
		},
		{
			name: "whitespace-only system message dropped",
			input: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "  "},
			},
			want: nil,
		},
		{
			name: "whitespace-only assistant message dropped",
			input: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "\t\n"},
			},
			want: nil,
		},
		{
			name: "assistant with empty content but tool calls is kept",
			input: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "", ToolCalls: []tools.ToolCall{{ID: "tc1"}}},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "", ToolCalls: []tools.ToolCall{{ID: "tc1"}}},
			},
		},
		{
			name: "tool result always forwarded even if whitespace-only",
			input: []chat.Message{
				{Role: chat.MessageRoleTool, Content: "   ", ToolCallID: "t1"},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleTool, Content: "   ", ToolCallID: "t1"},
			},
		},
		{
			name: "non-empty messages preserved verbatim including leading/trailing space",
			input: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "  hello  "},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "  hello  "},
			},
		},
		{
			name: "whitespace-only text part stripped from MultiContent",
			input: []chat.Message{
				{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "   "},
					img,
				}},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{img}},
			},
		},
		{
			name: "message dropped when all MultiContent parts are whitespace-only text",
			input: []chat.Message{
				{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "   "},
					{Type: chat.MessagePartTypeText, Text: "\t"},
				}},
			},
			want: nil,
		},
		{
			name: "image-only MultiContent message preserved",
			input: []chat.Message{
				{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{img}},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{img}},
			},
		},
		{
			name: "mix: valid and whitespace messages",
			input: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "be helpful"},
				{Role: chat.MessageRoleUser, Content: "  "},
				{Role: chat.MessageRoleUser, Content: "hello"},
				{Role: chat.MessageRoleTool, Content: "", ToolCallID: "t1"},
			},
			want: []chat.Message{
				{Role: chat.MessageRoleSystem, Content: "be helpful"},
				{Role: chat.MessageRoleUser, Content: "hello"},
				{Role: chat.MessageRoleTool, Content: "", ToolCallID: "t1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeMessageContent(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCompactionInput(t *testing.T) {
	t.Parallel()

	newMsg := func(role chat.MessageRole, content string) Item {
		return NewMessageItem(&Message{Message: chat.Message{Role: role, Content: content}})
	}

	t.Run("empty session returns empty", func(t *testing.T) {
		t.Parallel()
		sess := New()
		messages, sessIndices := sess.CompactionInput()
		assert.Empty(t, messages)
		assert.Empty(t, sessIndices)
	})

	t.Run("system messages on the session are filtered out", func(t *testing.T) {
		t.Parallel()
		sess := New(WithMessages([]Item{
			newMsg(chat.MessageRoleSystem, "sys"),
			newMsg(chat.MessageRoleUser, "u1"),
			newMsg(chat.MessageRoleAssistant, "a1"),
			newMsg(chat.MessageRoleSystem, "sys2"),
			newMsg(chat.MessageRoleUser, "u2"),
		}))

		messages, sessIndices := sess.CompactionInput()
		require.Len(t, messages, 3)
		assert.Equal(t, []int{1, 2, 4}, sessIndices)
		assert.Equal(t, "u1", messages[0].Content)
		assert.Equal(t, "a1", messages[1].Content)
		assert.Equal(t, "u2", messages[2].Content)
	})

	t.Run("prior summary surfaces synthetic message and starts at FirstKeptEntry", func(t *testing.T) {
		t.Parallel()
		items := []Item{
			newMsg(chat.MessageRoleUser, "u0"),
			newMsg(chat.MessageRoleAssistant, "a0"),
			newMsg(chat.MessageRoleUser, "u1-kept"),
			newMsg(chat.MessageRoleAssistant, "a1-kept"),
			{Summary: "prior summary", FirstKeptEntry: 2},
			newMsg(chat.MessageRoleUser, "u2"),
			newMsg(chat.MessageRoleAssistant, "a2"),
		}
		sess := New(WithMessages(items))

		messages, sessIndices := sess.CompactionInput()

		require.Len(t, messages, 5)
		assert.Equal(t, chat.MessageRoleUser, messages[0].Role)
		assert.Contains(t, messages[0].Content, "Session Summary: prior summary")
		// The synthetic message maps back to the prior summary item; the
		// kept-tail then resumes at the prior FirstKeptEntry, skipping
		// the (non-message) summary item itself.
		assert.Equal(t, []int{4, 2, 3, 5, 6}, sessIndices)
	})

	t.Run("prior summary without FirstKeptEntry starts strictly after the summary", func(t *testing.T) {
		t.Parallel()
		items := []Item{
			newMsg(chat.MessageRoleUser, "old"),
			newMsg(chat.MessageRoleAssistant, "old-reply"),
			{Summary: "prior summary"},
			newMsg(chat.MessageRoleUser, "new"),
			newMsg(chat.MessageRoleAssistant, "new-reply"),
		}
		sess := New(WithMessages(items))

		messages, sessIndices := sess.CompactionInput()

		require.Len(t, messages, 3)
		assert.Equal(t, []int{2, 3, 4}, sessIndices)
	})

	t.Run("returned messages are independent copies safe to mutate", func(t *testing.T) {
		t.Parallel()
		sess := New(WithMessages([]Item{
			NewMessageItem(&Message{Message: chat.Message{
				Role:         chat.MessageRoleUser,
				Content:      "hello",
				Cost:         1.5,
				CacheControl: true,
			}}),
		}))

		messages, _ := sess.CompactionInput()
		require.Len(t, messages, 1)

		messages[0].Cost = 0
		messages[0].CacheControl = false
		messages[0].Content = "mutated"

		assert.InDelta(t, 1.5, sess.Messages[0].Message.Message.Cost, 0)
		assert.True(t, sess.Messages[0].Message.Message.CacheControl)
		assert.Equal(t, "hello", sess.Messages[0].Message.Message.Content)
	})
}
