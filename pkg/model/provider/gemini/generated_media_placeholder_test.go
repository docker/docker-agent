package gemini

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// generatedMediaPlaceholderText mirrors the exact wording
// pkg/runtime's stripGeneratedMediaTransform produces for a single
// stripped artifact, so these tests exercise the actual
// runtime-normalized shape rather than an arbitrary placeholder string.
const generatedMediaPlaceholderText = "[Generated media omitted from history 1/1: cat.png (image/png)]"

// TestConvertMessagesToGemini_GeneratedMediaPlaceholder_MediaOnly verifies
// a media-only assistant turn whose generated artifact was stripped by
// pkg/runtime.stripGeneratedMediaTransform (Content and MultiContent both
// carry the placeholder text, MultiContent has no document part left)
// still produces a non-empty Gemini Content — the placeholder text part
// must survive the conversion, not just Content.
func TestConvertMessagesToGemini_GeneratedMediaPlaceholder_MediaOnly(t *testing.T) {
	t.Parallel()

	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "draw a cat"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: generatedMediaPlaceholderText,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText},
			},
		},
	}

	contents := convertMessagesToGemini(t.Context(), messages, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)

	require.Len(t, contents, 2, "the media-only placeholder turn must not be dropped")
	assistant := contents[1]
	assert.Equal(t, genai.RoleModel, assistant.Role)
	require.Len(t, assistant.Parts, 1)
	assert.Equal(t, generatedMediaPlaceholderText, assistant.Parts[0].Text)
}

// TestConvertMessagesToGemini_GeneratedMediaPlaceholder_Mixed verifies a
// mixed text+media assistant turn keeps its original text part AND gets
// the placeholder as an additional part, matching
// stripGeneratedMediaTransform's "keep original text, append placeholders"
// contract.
func TestConvertMessagesToGemini_GeneratedMediaPlaceholder_Mixed(t *testing.T) {
	t.Parallel()

	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "draw a cat"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here you go\n" + generatedMediaPlaceholderText,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here you go"},
				{Type: chat.MessagePartTypeText, Text: generatedMediaPlaceholderText},
			},
		},
	}

	contents := convertMessagesToGemini(t.Context(), messages, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)

	require.Len(t, contents, 2)
	assistant := contents[1]
	require.Len(t, assistant.Parts, 2, "original text part plus one placeholder part")
	assert.Equal(t, "here you go", assistant.Parts[0].Text)
	assert.Equal(t, generatedMediaPlaceholderText, assistant.Parts[1].Text)
}
