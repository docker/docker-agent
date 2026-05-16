package anthropic

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// minJPEG is a minimal JPEG magic-byte header for use in tests.
var minJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0}

// minPDF is a minimal PDF magic-byte header for use in tests.
var minPDF = []byte{0x25, 0x50, 0x44, 0x46, 0x2D} // %PDF-

// TestConvertDocumentAnthropic_StrategyB64_Image verifies that an image document
// with InlineData and a vision-capable model produces a native ImageBlockParam.
func TestConvertDocumentAnthropic_StrategyB64_Image(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	visionCaps := modelinfo.CapsWith(true, true)
	blocks, err := convertDocumentWithCaps(t.Context(), doc, visionCaps)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "expected exactly one block")
	require.NotNil(t, blocks[0].OfImage, "expected image block")
	assert.Nil(t, blocks[0].OfText, "expected no text block for image")
}

// TestConvertDocumentAnthropic_StrategyB64_PDF verifies that a PDF document
// produces a native BetaRequestDocumentBlock when the model supports PDFs.
func TestConvertDocumentAnthropic_StrategyB64_PDF(t *testing.T) {
	doc := chat.Document{
		Name:     "spec.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{InlineData: minPDF},
	}

	pdfCaps := modelinfo.CapsWith(true, true)
	blocks, err := convertDocumentWithCaps(t.Context(), doc, pdfCaps)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "expected exactly one block")
	require.NotNil(t, blocks[0].OfDocument, "expected document block for PDF")
	assert.Nil(t, blocks[0].OfText, "expected no text block for PDF")
}

// TestConvertDocumentAnthropic_QualifiedIDRequired is the regression test for
// the bug where convertUserMultiContent passed only c.ModelConfig.Model (bare
// model name) to convertDocument instead of c.ModelConfig.Provider+"/"+c.ModelConfig.Model.
// When the bare name was used, modelinfo.LoadCaps always missed the model and all
// image/PDF attachments were silently dropped.
//
// The test constructs a Client with Provider="anthropic" and Model="claude-sonnet-4-6",
// injects a fake modelsdev.Store, and calls convertUserMultiContent directly.
// The image block must be present in the output — which only happens if the
// fully-qualified "anthropic/claude-sonnet-4-6" was used for the caps lookup.
func TestConvertDocumentAnthropic_QualifiedIDRequired(t *testing.T) {
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-6": {
						Modalities: modelsdev.Modalities{
							Input: []string{"text", "image", "pdf"},
						},
					},
				},
			},
		},
	})

	var modelOpts options.ModelOptions
	options.WithModelsDevStore(store)(&modelOpts)

	c := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-sonnet-4-6",
			},
			ModelOptions: modelOpts,
		},
	}

	parts := []chat.MessagePart{
		{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     "photo.jpg",
				MimeType: "image/jpeg",
				Source:   chat.DocumentSource{InlineData: minJPEG},
			},
		},
	}

	blocks, err := c.convertUserMultiContent(t.Context(), parts)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "image must not be dropped when provider+model ID is used for caps lookup")
	assert.NotNil(t, blocks[0].OfImage, "expected native image block")
}

func TestConvertDocumentAnthropic_StrategyTXT(t *testing.T) {
	doc := chat.Document{
		Name:     "spec.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "## Specification"},
	}

	blocks, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].OfText)
	assert.Contains(t, blocks[0].OfText.Text, "spec-md")
	assert.Contains(t, blocks[0].OfText.Text, "text-markdown")
	assert.Contains(t, blocks[0].OfText.Text, "## Specification")
}

func TestConvertDocumentAnthropic_StrategyTXT_Envelope(t *testing.T) {
	doc := chat.Document{
		Name:     "notes.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{InlineText: "some notes"},
	}

	blocks, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].OfText)
	text := blocks[0].OfText.Text
	assert.True(t, strings.HasPrefix(text, "<document"), "should be wrapped in envelope")
}

func TestConvertDocumentAnthropic_Drop_NoContent(t *testing.T) {
	doc := chat.Document{
		Name:     "empty.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{},
	}

	blocks, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	assert.Nil(t, blocks, "should be dropped when no inline content")
}

func TestConvertDocumentAnthropic_Drop_UnsupportedMIME(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	textOnlyCaps := modelinfo.CapsWith(false, false)
	blocks, err := convertDocumentWithCaps(t.Context(), doc, textOnlyCaps)
	require.NoError(t, err)
	assert.Nil(t, blocks, "image should be dropped for text-only model")
}
