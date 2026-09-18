package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestOpenAIResponseStatePersistence(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithTitle("state"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	state := &chat.OpenAIResponse{
		ID: "resp_1", Source: "openai/gpt-5.6", SessionID: sess.ID,
		Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`)},
	}
	message := &Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "done", OpenAIResponse: state}}
	_, err := store.AddMessage(t.Context(), sess.ID, message)
	require.NoError(t, err)
	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	restored := got.Messages[0].Message.Message.OpenAIResponse
	require.NotNil(t, restored)
	assert.Equal(t, state, restored)

	clone := cloneChatMessage(message.Message)
	clone.OpenAIResponse.ID = "another"
	clone.OpenAIResponse.Output[0][0] = '['
	assert.Equal(t, "resp_1", state.ID)
	assert.Equal(t, byte('{'), state.Output[0][0], "snapshot must not alias opaque output bytes")
}
