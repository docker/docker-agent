package oaistream

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// generatedMediaPlaceholderText mirrors the exact wording
// pkg/runtime's stripGeneratedMediaTransform produces for a single
// stripped artifact, so these tests exercise the actual
// runtime-normalized shape rather than an arbitrary placeholder string.
const generatedMediaPlaceholderText = "[Generated media omitted from history 1/1: cat.png (image/png)]"

// TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_MediaOnly verifies
// a media-only assistant turn whose generated artifact was stripped by
// pkg/runtime.stripGeneratedMediaTransform still produces a non-empty
// OpenAI assistant message: convertMessagesWithCaps treats a non-empty
// MultiContent as authoritative and ignores Content entirely for
// assistant messages in that case (see messages.go), so the placeholder
// MUST exist as a MultiContent text part, not just in Content, or the
// turn would convert to an empty content array.
func TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_MediaOnly(t *testing.T) {
	t.Parallel()

	messages := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: generatedMediaPlaceholderText,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText},
			},
		},
	}

	out := ConvertMessagesWithCaps(t.Context(), messages, modelinfo.ModelCapabilities{})
	require.Len(t, out, 1, "the media-only placeholder turn must not be dropped")

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	content, ok := m["content"].([]any)
	require.True(t, ok, "assistant content must be the array-of-parts form, not a bare string")
	require.Len(t, content, 1)
	part, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, generatedMediaPlaceholderText, part["text"])
}

// TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_Mixed verifies a
// mixed text+media assistant turn keeps its original text part AND gets
// the placeholder as an additional part.
func TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_Mixed(t *testing.T) {
	t.Parallel()

	messages := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here you go\n" + generatedMediaPlaceholderText,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here you go"},
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText},
			},
		},
	}

	out := ConvertMessagesWithCaps(t.Context(), messages, modelinfo.ModelCapabilities{})
	require.Len(t, out, 1)

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	content, ok := m["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2, "original text part plus one placeholder part")
	first, _ := content[0].(map[string]any)
	second, _ := content[1].(map[string]any)
	assert.Equal(t, "here you go", first["text"])
	assert.Equal(t, generatedMediaPlaceholderText, second["text"])
}
