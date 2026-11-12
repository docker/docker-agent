package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestProviderStatePersistsOpaqueBytes(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithTitle("state"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	// Redacted reasoning bytes are base64 inside the raw block JSON.
	content := json.RawMessage(`[{"type":"redacted_reasoning","data":"/wCA"},{"type":"text","text":"ok"}]`)
	msg := chat.Message{Role: chat.MessageRoleAssistant, Content: "ok"}
	msg.AttachProviderState(&chat.ProviderState{Provider: "amazon-bedrock", Content: content})
	_, err := store.AddMessage(t.Context(), sess.ID, &Message{Message: msg})
	require.NoError(t, err)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	restored := got.Messages[0].Message.Message
	state := restored.ReplayableProviderState("amazon-bedrock")
	require.NotNil(t, state, "a reloaded message stays replayable")
	assert.JSONEq(t, string(content), string(state.Content))

	var blocks []struct {
		Data []byte `json:"data"`
	}
	require.NoError(t, json.Unmarshal(state.Content, &blocks))
	assert.Equal(t, []byte{0xff, 0x00, 0x80}, blocks[0].Data)
}
