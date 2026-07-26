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

// generatedMediaPlaceholderText1/2 are the exact runtime-normalized
// per-artifact placeholder strings for a TWO-artifact turn, in source
// order — the review's "robust multi-artifact" regression exercises
// conversion of these exact shapes rather than a single-artifact
// placeholder.
const (
	generatedMediaPlaceholderText1 = "[Generated media omitted from history 1/2: cat.png (image/png)]"
	generatedMediaPlaceholderText2 = "[Generated media omitted from history 2/2: dog.jpg (image/jpeg)]"
)

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

// TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_MultipleArtifacts_MediaOnly
// is the review's "robust multi-artifact placeholder" regression: a
// media-only turn with TWO stripped artifacts must convert to exactly two
// OpenAI content parts, one per artifact, in source order — never one
// combined part and never a dropped/empty content array.
func TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_MultipleArtifacts_MediaOnly(t *testing.T) {
	t.Parallel()

	messages := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: generatedMediaPlaceholderText1 + "\n" + generatedMediaPlaceholderText2,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText1},
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText2},
			},
		},
	}

	out := ConvertMessagesWithCaps(t.Context(), messages, modelinfo.ModelCapabilities{})
	require.Len(t, out, 1, "the media-only multi-artifact placeholder turn must not be dropped")

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	content, ok := m["content"].([]any)
	require.True(t, ok, "assistant content must be the array-of-parts form, not a bare string")
	require.Len(t, content, 2, "one content part per stripped artifact, in source order")
	first, _ := content[0].(map[string]any)
	second, _ := content[1].(map[string]any)
	assert.Equal(t, generatedMediaPlaceholderText1, first["text"])
	assert.Equal(t, generatedMediaPlaceholderText2, second["text"])
}

// TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_MultipleArtifacts_Mixed
// is the mixed text+multi-artifact-media counterpart.
func TestConvertMessagesWithCaps_GeneratedMediaPlaceholder_MultipleArtifacts_Mixed(t *testing.T) {
	t.Parallel()

	messages := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here you go\n" + generatedMediaPlaceholderText1 + "\n" + generatedMediaPlaceholderText2,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here you go"},
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText1},
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText2},
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
	require.Len(t, content, 3, "original text part plus one placeholder part per stripped artifact")
	first, _ := content[0].(map[string]any)
	second, _ := content[1].(map[string]any)
	third, _ := content[2].(map[string]any)
	assert.Equal(t, "here you go", first["text"])
	assert.Equal(t, generatedMediaPlaceholderText1, second["text"])
	assert.Equal(t, generatedMediaPlaceholderText2, third["text"])
}
