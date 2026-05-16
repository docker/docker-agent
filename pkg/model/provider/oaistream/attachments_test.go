package oaistream

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// minJPEG is a minimal JPEG magic-byte header for use in tests.
var minJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0}

// TestConvertDocument_StrategyB64_Image verifies that an image document with
// InlineData and a vision-capable model produces an image content part with
// a data-URI, not a text part.
func TestConvertDocument_StrategyB64_Image(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	visionCaps := modelinfo.CapsWith(true, true)
	parts, err := convertDocumentWithCaps(t.Context(), doc, visionCaps)
	require.NoError(t, err)
	require.Len(t, parts, 1, "expected exactly one image part")
	require.NotNil(t, parts[0].OfImageURL, "expected image part, got non-image")
	assert.Nil(t, parts[0].OfText, "expected no text part for B64 image")

	// Data URI must embed the base64-encoded payload.
	wantB64 := base64.StdEncoding.EncodeToString(minJPEG)
	assert.Contains(t, parts[0].OfImageURL.ImageURL.URL, "data:image/jpeg;base64,")
	assert.Contains(t, parts[0].OfImageURL.ImageURL.URL, wantB64)
}

// TestConvertDocument_StrategyB64_ImageDropped verifies that an image is
// dropped when the model does not support vision.
func TestConvertDocument_StrategyB64_ImageDropped(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	textOnlyCaps := modelinfo.CapsWith(false, false)
	parts, err := convertDocumentWithCaps(t.Context(), doc, textOnlyCaps)
	require.NoError(t, err)
	assert.Nil(t, parts, "image should be dropped for text-only model")
}

// TestConvertDocument_QualifiedIDRequired is the regression test for the bug
// where callers passed a bare model name instead of a "provider/model" ID,
// causing modelinfo to miss the model and silently drop image/PDF attachments.
//
// It calls ConvertMultiContent with an injected fake store, exercising
// the same path as the production client (which calls ConvertMessages with c.ID()).
func TestConvertDocument_QualifiedIDRequired(t *testing.T) {
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"openai": {
				Models: map[string]modelsdev.Model{
					"gpt-4o": {
						Modalities: modelsdev.Modalities{
							Input: []string{"text", "image"},
						},
					},
				},
			},
		},
	})

	msgParts := []chat.MessagePart{{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "photo.jpg",
			MimeType: "image/jpeg",
			Source:   chat.DocumentSource{InlineData: minJPEG},
		},
	}}

	// Bare model name (the original bug): image must be dropped.
	partsBare := ConvertMultiContent(t.Context(), msgParts, modelsdev.NewID("", "gpt-4o"), store)
	assert.Empty(t, partsBare, "bare model name must not resolve caps: image should be dropped")

	// Qualified ID (the fix, matching what c.ID() returns): image must be preserved.
	partsQualified := ConvertMultiContent(t.Context(), msgParts, modelsdev.NewID("openai", "gpt-4o"), store)
	require.Len(t, partsQualified, 1, "qualified ID must resolve caps: image should be present")
	assert.NotNil(t, partsQualified[0].OfImageURL, "expected image URL part for qualified model ID")
}

func TestConvertDocument_StrategyTXT(t *testing.T) {
	doc := chat.Document{
		Name:     "readme.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "# Hello World"},
	}

	parts, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].OfText)
	assert.Contains(t, parts[0].OfText.Text, "readme-md")
	assert.Contains(t, parts[0].OfText.Text, "text-markdown")
	assert.Contains(t, parts[0].OfText.Text, "# Hello World")
}

func TestConvertDocument_StrategyTXT_Envelope(t *testing.T) {
	doc := chat.Document{
		Name:     "data.csv",
		MimeType: "text/csv",
		Source:   chat.DocumentSource{InlineText: "a,b,c\n1,2,3"},
	}

	parts, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].OfText)
	text := parts[0].OfText.Text
	assert.True(t, strings.HasPrefix(text, "<document"), "should be wrapped in document envelope")
}

func TestConvertDocument_Drop_NoContent(t *testing.T) {
	doc := chat.Document{
		Name:     "empty.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{},
	}

	parts, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	assert.Nil(t, parts, "should be dropped when no inline content")
}
