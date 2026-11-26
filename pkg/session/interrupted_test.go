package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
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
