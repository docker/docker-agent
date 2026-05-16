package bedrock

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// minJPEG is a minimal JPEG magic-byte header for use in tests.
var minJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0}

// minPDF is a minimal PDF magic-byte header for use in tests.
var minPDF = []byte{0x25, 0x50, 0x44, 0x46, 0x2D} // %PDF-

// TestConvertDocumentBedrock_StrategyB64_Image verifies that an image document
// with InlineData and a vision-capable model produces a ContentBlockMemberImage.
func TestConvertDocumentBedrock_StrategyB64_Image(t *testing.T) {
	doc := chat.Document{
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Source:   chat.DocumentSource{InlineData: minJPEG},
	}

	visionCaps := modelinfo.CapsWith(true, true)
	blocks, err := convertDocumentWithCaps(t.Context(), doc, visionCaps)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "expected exactly one block")
	imageBlock, ok := blocks[0].(*types.ContentBlockMemberImage)
	require.True(t, ok, "expected ContentBlockMemberImage, got %T", blocks[0])
	assert.Equal(t, types.ImageFormatJpeg, imageBlock.Value.Format)
	srcBytes, ok := imageBlock.Value.Source.(*types.ImageSourceMemberBytes)
	require.True(t, ok, "expected ImageSourceMemberBytes")
	assert.Equal(t, minJPEG, srcBytes.Value)
}

// TestConvertDocumentBedrock_StrategyB64_PDF verifies that a PDF document
// produces a ContentBlockMemberDocument when the model supports PDFs.
func TestConvertDocumentBedrock_StrategyB64_PDF(t *testing.T) {
	doc := chat.Document{
		Name:     "spec.pdf",
		MimeType: "application/pdf",
		Source:   chat.DocumentSource{InlineData: minPDF},
	}

	pdfCaps := modelinfo.CapsWith(true, true)
	blocks, err := convertDocumentWithCaps(t.Context(), doc, pdfCaps)
	require.NoError(t, err)
	require.Len(t, blocks, 1, "expected exactly one block")
	docBlock, ok := blocks[0].(*types.ContentBlockMemberDocument)
	require.True(t, ok, "expected ContentBlockMemberDocument, got %T", blocks[0])
	assert.Equal(t, types.DocumentFormatPdf, docBlock.Value.Format)
}

// TestConvertDocumentBedrock_StrategyB64_ImageDropped verifies that an image
// is dropped when the model does not support vision.
func TestConvertDocumentBedrock_StrategyB64_ImageDropped(t *testing.T) {
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

func TestConvertDocumentBedrock_StrategyTXT(t *testing.T) {
	doc := chat.Document{
		Name:     "notes.md",
		MimeType: "text/markdown",
		Source:   chat.DocumentSource{InlineText: "## Notes"},
	}

	blocks, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	require.True(t, ok, "expected text block for TXT strategy")
	assert.Contains(t, textBlock.Value, "notes-md")
	assert.Contains(t, textBlock.Value, "text-markdown")
	assert.Contains(t, textBlock.Value, "## Notes")
}

func TestConvertDocumentBedrock_StrategyTXT_Envelope(t *testing.T) {
	doc := chat.Document{
		Name:     "data.csv",
		MimeType: "text/csv",
		Source:   chat.DocumentSource{InlineText: "a,b"},
	}

	blocks, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	require.True(t, ok, "expected text block")
	assert.True(t, strings.HasPrefix(textBlock.Value, "<document"), "should be wrapped in envelope")
}

func TestConvertDocumentBedrock_Drop_NoContent(t *testing.T) {
	doc := chat.Document{
		Name:     "empty.txt",
		MimeType: "text/plain",
		Source:   chat.DocumentSource{},
	}

	blocks, err := convertDocumentWithCaps(t.Context(), doc, modelinfo.ModelCapabilities{})
	require.NoError(t, err)
	assert.Nil(t, blocks, "should be nil when no inline content")
}

func TestSanitizeDocumentName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"report.pdf", "report"},
		{"my-document.docx", "my-document"},
		{"hello world.txt", "hello world"},
		{"file.with.dots.pdf", "file-with-dots"},
		{".pdf", "pdf"}, // edge case: name is only extension
		{"", "document"},
		{"123.xlsx", "123"},
		{"report (draft).pdf", "report (draft)"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeDocumentName(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeDocumentName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
