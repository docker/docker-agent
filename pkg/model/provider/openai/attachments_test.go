package openai

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// minJPEG is a minimal JPEG magic-byte header for use in tests.
var minJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0}

// TestConvertDocumentResponseInput_StrategyB64_Image verifies that an image
// document with InlineData and a vision-capable model produces a native image
// part (OfInputImage) with a data-URI.
func TestConvertDocumentResponseInput_StrategyB64_Image(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	visionCaps := modelinfo.CapsWith(true, true)
	parts, err := convertDocumentToResponseInputWithCaps(t.Context(), doc, visionCaps)
	require.NoError(t, err)
	require.Len(t, parts, 1, "expected exactly one image part")
	require.NotNil(t, parts[0].OfInputImage, "expected OfInputImage, not text")
	assert.Nil(t, parts[0].OfInputText, "expected no text part for B64 image")

	wantB64 := base64.StdEncoding.EncodeToString(minJPEG)
	imageURL := parts[0].OfInputImage.ImageURL.Value
	assert.Contains(t, imageURL, "data:image/jpeg;base64,")
	assert.Contains(t, imageURL, wantB64)
}

// TestConvertDocumentResponseInput_StrategyB64_ImageDropped verifies that an
// image is dropped for a text-only model.
func TestConvertDocumentResponseInput_StrategyB64_ImageDropped(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	textOnlyCaps := modelinfo.CapsWith(false, false)
	parts, err := convertDocumentToResponseInputWithCaps(t.Context(), doc, textOnlyCaps)
	require.NoError(t, err)
	assert.Nil(t, parts, "image should be dropped for text-only model")
}

func TestConvertDocumentResponseInput_StrategyTXT(t *testing.T) {
	doc := chat.Document{
		Name:     "spec.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "## API Spec"},
	}

	parts, err := convertDocumentToResponseInputWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].OfInputText)
	text := parts[0].OfInputText.Text
	assert.Contains(t, text, "spec-md")
	assert.Contains(t, text, "text-markdown")
	assert.Contains(t, text, "## API Spec")
}

func TestConvertDocumentResponseInput_StrategyTXT_Envelope(t *testing.T) {
	doc := chat.Document{
		Name:     "data.csv",
		MimeType: "text/csv",
		Source:   chat.DocumentSource{InlineText: "x,y"},
	}

	parts, err := convertDocumentToResponseInputWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].OfInputText)
	text := parts[0].OfInputText.Text
	assert.True(t, strings.HasPrefix(text, "<document"), "should be wrapped in envelope")
}

func TestConvertDocumentResponseInput_Drop_NoContent(t *testing.T) {
	doc := chat.Document{
		Name:     "empty.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{},
	}

	parts, err := convertDocumentToResponseInputWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	assert.Nil(t, parts, "should be nil when no inline content")
}
