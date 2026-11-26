package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestIncompleteAssistantMessageStaysOutOfModelContext(t *testing.T) {
	t.Parallel()
	sess := New()
	sess.AddMessage(UserMessage("hi"))
	sess.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "cut off he", FinishReason: chat.FinishReasonIncomplete}})

	var conversation []chat.Message
	for _, msg := range sess.GetMessages(agent.New("root", "instructions")) {
		if msg.Role != chat.MessageRoleSystem {
			conversation = append(conversation, msg)
		}
	}
	require.Len(t, conversation, 1)
	assert.Equal(t, chat.MessageRoleUser, conversation[0].Role)
	assert.Len(t, sess.GetAllMessages(), 2, "the transcript keeps the cut-off text")
	assert.Empty(t, sess.GetLastAssistantMessageContent())
	assert.Len(t, sess.VisibleExchanges(), 1, "routing hooks do not see it either")
}

func TestUnanswered(t *testing.T) {
	t.Parallel()
	assistant := func(finish chat.FinishReason, content string, calls ...string) *Message {
		msg := &Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: content, FinishReason: finish}}
		for _, id := range calls {
			msg.Message.ToolCalls = append(msg.Message.ToolCalls, tools.ToolCall{ID: id})
		}
		return msg
	}
	toolResult := &Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "c1", Content: "ok"}}

	tests := []struct {
		name     string
		messages []*Message
		want     bool
		// prompt is the user's message that nothing has answered yet, if any.
		prompt string
	}{
		{name: "empty transcript"},
		{name: "prompt", messages: []*Message{UserMessage("hi")}, want: true, prompt: "hi"},
		{name: "answer", messages: []*Message{UserMessage("hi"), assistant(chat.FinishReasonStop, "hello")}},
		{
			name: "empty answer",
			messages: []*Message{UserMessage("hi"), {Message: chat.Message{
				Role: chat.MessageRoleAssistant, FinishReason: chat.FinishReasonStop, Usage: &chat.Usage{OutputTokens: 1},
			}}},
		},
		{name: "tool call without result", messages: []*Message{UserMessage("hi"), assistant(chat.FinishReasonToolCalls, "", "c1")}, want: true},
		{name: "tool result", messages: []*Message{UserMessage("hi"), assistant(chat.FinishReasonToolCalls, "", "c1"), toolResult}, want: true},
		{
			name:     "answer cut off mid-generation",
			messages: []*Message{UserMessage("hi"), assistant(chat.FinishReasonIncomplete, "hel")},
			want:     true,
			prompt:   "hi",
		},
		{
			name:     "cut-off text after an answer",
			messages: []*Message{UserMessage("hi"), assistant(chat.FinishReasonStop, "hello"), assistant(chat.FinishReasonIncomplete, "and")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sess := New()
			for _, msg := range tt.messages {
				sess.AddMessage(msg)
			}
			assert.Equal(t, tt.want, sess.Unanswered())
			prompt, _ := sess.UnansweredPrompt()
			assert.Equal(t, tt.prompt, prompt.Content)
		})
	}
}
