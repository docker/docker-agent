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
	t.Parallel()
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	visionCaps := modelinfo.CapsWith(true, true, false, false)
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

// TestConvertDocument_StrategyB64_PDF verifies that a PDF document with
// InlineData and a PDF-capable model produces a `file` content part carrying a
// base64 data URI, rather than being dropped.
func TestConvertDocument_StrategyB64_PDF(t *testing.T) {
	t.Parallel()
	pdf := []byte("%PDF-1.4 minimal")
	doc := chat.Document{
		Name:     "spec.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{InlineData: pdf},
	}

	pdfCaps := modelinfo.CapsWith(false, true, false, false)
	parts, err := convertDocumentWithCaps(t.Context(), doc, pdfCaps)
	require.NoError(t, err)
	require.Len(t, parts, 1, "expected exactly one file part")
	require.NotNil(t, parts[0].OfFile, "expected file part for PDF, got non-file")
	assert.Nil(t, parts[0].OfImageURL, "PDF must not be an image part")

	wantB64 := base64.StdEncoding.EncodeToString(pdf)
	assert.Equal(t, "spec.pdf", parts[0].OfFile.File.Filename.Value)
	assert.Contains(t, parts[0].OfFile.File.FileData.Value, "data:application/pdf;base64,")
	assert.Contains(t, parts[0].OfFile.File.FileData.Value, wantB64)
}

// TestConvertDocument_StrategyB64_PDFDropped verifies that a PDF is dropped when
// the model does not declare PDF support.
func TestConvertDocument_StrategyB64_PDFDropped(t *testing.T) {
	t.Parallel()
	doc := chat.Document{
		Name:     "spec.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{InlineData: []byte("%PDF-1.4")},
	}

	parts, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.CapsWith(true, false, false, false))
	require.NoError(t, err)
	assert.Nil(t, parts, "pdf should be dropped when the model does not support PDF")
}

// TestConvertMessagesWithCaps verifies the caps-injectable message path: an
// image attachment is forwarded when the injected caps allow it and dropped
// otherwise, independent of any models.dev store.
func TestConvertMessagesWithCaps(t *testing.T) {
	t.Parallel()
	messages := []chat.Message{{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     "photo.jpg",
				MimeType: "image/jpeg",
				Source:   chat.DocumentSource{InlineData: minJPEG},
			},
		}},
	}}

	withVision := ConvertMessagesWithCaps(t.Context(), messages, modelinfo.CapsWith(true, false, false, false))
	require.Len(t, withVision, 1)
	require.NotNil(t, withVision[0].OfUser)
	require.Len(t, withVision[0].OfUser.Content.OfArrayOfContentParts, 1)
	assert.NotNil(t, withVision[0].OfUser.Content.OfArrayOfContentParts[0].OfImageURL)

	textOnly := ConvertMessagesWithCaps(t.Context(), messages, modelinfo.ModelCapabilities{})
	require.Len(t, textOnly, 1)
	require.NotNil(t, textOnly[0].OfUser)
	assert.Empty(t, textOnly[0].OfUser.Content.OfArrayOfContentParts, "image should be dropped for text-only caps")

	// Symmetric PDF case: a PDF is forwarded as a file part when PDF caps are
	// injected, and dropped otherwise.
	pdfMessages := []chat.Message{{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     "spec.pdf",
				MimeType: "application/pdf",
				Source:   chat.DocumentSource{InlineData: []byte("%PDF-1.4")},
			},
		}},
	}}

	withPDF := ConvertMessagesWithCaps(t.Context(), pdfMessages, modelinfo.CapsWith(false, true, false, false))
	require.Len(t, withPDF, 1)
	require.NotNil(t, withPDF[0].OfUser)
	require.Len(t, withPDF[0].OfUser.Content.OfArrayOfContentParts, 1)
	assert.NotNil(t, withPDF[0].OfUser.Content.OfArrayOfContentParts[0].OfFile)

	pdfDropped := ConvertMessagesWithCaps(t.Context(), pdfMessages, modelinfo.ModelCapabilities{})
	require.Len(t, pdfDropped, 1)
	require.NotNil(t, pdfDropped[0].OfUser)
	assert.Empty(t, pdfDropped[0].OfUser.Content.OfArrayOfContentParts, "pdf should be dropped for text-only caps")
}

// TestConvertDocument_StrategyB64_ImageDropped verifies that an image is
// dropped when the model does not support vision.
func TestConvertDocument_StrategyB64_ImageDropped(t *testing.T) {
	t.Parallel()
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	textOnlyCaps := modelinfo.CapsWith(false, false, false, false)
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
	t.Parallel()
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
	partsBare := ConvertMultiContent(t.Context(), msgParts, modelsdev.NewID("", "gpt-4o"), store, nil)
	assert.Empty(t, partsBare, "bare model name must not resolve caps: image should be dropped")

	// Qualified ID (the fix, matching what c.ID() returns): image must be preserved.
	partsQualified := ConvertMultiContent(t.Context(), msgParts, modelsdev.NewID("openai", "gpt-4o"), store, nil)
	require.Len(t, partsQualified, 1, "qualified ID must resolve caps: image should be present")
	assert.NotNil(t, partsQualified[0].OfImageURL, "expected image URL part for qualified model ID")
}

func TestConvertDocument_StrategyTXT(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	doc := chat.Document{
		Name:     "empty.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{},
	}

	parts, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	assert.Nil(t, parts, "should be dropped when no inline content")
}
