package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// generatedMediaPlaceholderText mirrors the exact wording
// pkg/runtime's stripGeneratedMediaTransform mirrors into Content for a
// single stripped artifact, so these tests exercise the actual
// runtime-normalized shape rather than an arbitrary placeholder string.
const generatedMediaPlaceholderText = "[Generated media omitted from history 1/1: cat.png (image/png)]"

// mediaOnlyPlaceholderMessage is the exact shape
// pkg/runtime.stripGeneratedMediaTransform produces for a media-only
// assistant turn once its generated artifact is stripped: Content carries
// the placeholder, and MultiContent carries the same text as a mirrored
// Text part (never just Content with an empty MultiContent, and never just
// MultiContent with an empty Content — see that transform's doc comment).
func mediaOnlyPlaceholderMessage() chat.Message {
	return chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: generatedMediaPlaceholderText,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText},
		},
	}
}

// mixedTextPlaceholderMessage is the exact shape
// pkg/runtime.stripGeneratedMediaTransform produces for a mixed text+media
// assistant turn: the original text is kept (both in Content, prefixed,
// and as MultiContent's first part, untouched) and the placeholder is
// appended to both.
func mixedTextPlaceholderMessage() chat.Message {
	return chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "here you go\n" + generatedMediaPlaceholderText,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "here you go"},
			{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText},
		},
	}
}

// TestConvertMessages_GeneratedMediaPlaceholder_MediaOnly is the residual-
// caveat regression test (Step 4 remediation): the legacy (non-beta)
// Anthropic converter reads only msg.Content for assistant text — it never
// looks at MultiContent's text parts — so a media-only turn whose
// placeholder existed ONLY in MultiContent would convert to a
// content-less assistant message and get dropped entirely (len(contentBlocks)
// == 0). Because stripGeneratedMediaTransform mirrors the placeholder into
// Content too, the turn must survive here.
func TestConvertMessages_GeneratedMediaPlaceholder_MediaOnly(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{mediaOnlyPlaceholderMessage()}

	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 1, "the media-only placeholder turn must not be dropped")

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "assistant", m["role"])
	content, ok := m["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	cb, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", cb["type"])
	assert.Equal(t, generatedMediaPlaceholderText, cb["text"])
}

// TestConvertMessages_GeneratedMediaPlaceholder_Mixed verifies the legacy
// converter's text block carries BOTH the original text and the appended
// placeholder for a mixed text+media turn.
func TestConvertMessages_GeneratedMediaPlaceholder_Mixed(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{mixedTextPlaceholderMessage()}

	out, err := testClient().convertMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	b, err := json.Marshal(out[0])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	content, ok := m["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	cb, ok := content[0].(map[string]any)
	require.True(t, ok)
	text, _ := cb["text"].(string)
	assert.Contains(t, text, "here you go")
	assert.Contains(t, text, generatedMediaPlaceholderText)
}

// TestConvertBetaMessages_GeneratedMediaPlaceholder_MediaOnly is the beta
// (extended-thinking) client's counterpart to
// TestConvertMessages_GeneratedMediaPlaceholder_MediaOnly: convertBetaMessages
// also reads only msg.Content for assistant text, so it is independently
// vulnerable to the same drop if the placeholder only existed in
// MultiContent.
func TestConvertBetaMessages_GeneratedMediaPlaceholder_MediaOnly(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{mediaOnlyPlaceholderMessage()}

	out, err := testClient().convertBetaMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 1, "the media-only placeholder turn must not be dropped")
	require.Len(t, out[0].Content, 1)
	require.NotNil(t, out[0].Content[0].OfText)
	assert.Equal(t, generatedMediaPlaceholderText, out[0].Content[0].OfText.Text)
}

// TestConvertBetaMessages_GeneratedMediaPlaceholder_Mixed is the beta
// client's counterpart to TestConvertMessages_GeneratedMediaPlaceholder_Mixed.
func TestConvertBetaMessages_GeneratedMediaPlaceholder_Mixed(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{mixedTextPlaceholderMessage()}

	out, err := testClient().convertBetaMessages(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Content, 1)
	require.NotNil(t, out[0].Content[0].OfText)
	text := out[0].Content[0].OfText.Text
	assert.Contains(t, text, "here you go")
	assert.Contains(t, text, generatedMediaPlaceholderText)
}
