package gemini

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// minJPEG is a minimal JPEG magic-byte header for use in tests.
var minJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0}

// TestConvertDocumentGemini_StrategyB64_Image verifies that an image document
// with InlineData and a vision-capable model produces a Blob part (not a text part).
func TestConvertDocumentGemini_StrategyB64_Image(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	visionCaps := modelinfo.CapsWith(true, true)
	part, err := convertDocumentWithCaps(t.Context(), doc, visionCaps)
	require.NoError(t, err)
	require.NotNil(t, part, "expected a non-nil part for B64 image")
	// For a blob part the Text field is empty; the inline blob carries the data.
	assert.Empty(t, part.Text, "expected blob part, not text part")
	assert.Equal(t, minJPEG, part.InlineData.Data, "inline data should match input bytes")
	assert.Equal(t, "image/jpeg", part.InlineData.MIMEType)
}

// TestConvertDocumentGemini_StrategyB64_ImageDropped verifies that an image is
// dropped when the model does not support vision.
func TestConvertDocumentGemini_StrategyB64_ImageDropped(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	textOnlyCaps := modelinfo.CapsWith(false, false)
	part, err := convertDocumentWithCaps(t.Context(), doc, textOnlyCaps)
	require.NoError(t, err)
	assert.Nil(t, part, "image should be dropped for text-only model")
}

func TestConvertDocumentGemini_StrategyTXT(t *testing.T) {
	doc := chat.Document{
		Name:     "readme.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "# Read Me"},
	}

	part, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.NotNil(t, part)
	assert.Contains(t, part.Text, "readme-md")
	assert.Contains(t, part.Text, "text-markdown")
	assert.Contains(t, part.Text, "# Read Me")
}

func TestConvertDocumentGemini_StrategyTXT_Envelope(t *testing.T) {
	doc := chat.Document{
		Name:     "data.csv",
		MimeType: "text/csv",
		Source:   chat.DocumentSource{InlineText: "col1,col2"},
	}

	part, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.NotNil(t, part)
	assert.True(t, strings.HasPrefix(part.Text, "<document"), "should be wrapped in envelope")
}

func TestConvertDocumentGemini_Drop_NoContent(t *testing.T) {
	doc := chat.Document{
		Name:     "empty.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{},
	}

	part, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	assert.Nil(t, part, "should be nil when no inline content")
}
