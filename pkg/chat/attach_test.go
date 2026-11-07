package chat_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func encodeJPEGBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func encodePNGBytes(w, h int, alpha bool) []byte {
	if alpha {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				img.Set(x, y, color.NRGBA{R: 0, G: 128, B: 255, A: 128})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			panic(err)
		}
		return buf.Bytes()
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 0, G: 128, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func writeTempFile(t *testing.T, ext string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "attach-*"+ext)
	require.NoError(t, err)
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachmentWithMetadata — MessagePartTypeFile
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachmentWithMetadata_JPEG_Passthrough(t *testing.T) {
	t.Parallel()
	data := encodeJPEGBytes(100, 100)
	path := writeTempFile(t, ".jpg", data)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/jpeg"},
	})
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", doc.MimeType)
	assert.NotEmpty(t, doc.Source.InlineData)
	assert.Empty(t, doc.Source.InlineText)
	assert.Equal(t, filepath.Base(path), doc.Name)
}

func TestProcessAttachmentWithMetadata_PNG_Passthrough(t *testing.T) {
	t.Parallel()
	data := encodePNGBytes(100, 100, false)
	path := writeTempFile(t, ".png", data)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/png"},
	})
	require.NoError(t, err)
	assert.Equal(t, "image/png", doc.MimeType)
	assert.NotEmpty(t, doc.Source.InlineData)
}

func TestProcessAttachmentWithMetadata_PNG_WithAlpha_StaysPNG(t *testing.T) {
	t.Parallel()
	data := encodePNGBytes(100, 100, true)
	path := writeTempFile(t, ".png", data)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/png"},
	})
	require.NoError(t, err)
	assert.True(t, doc.MimeType == "image/png" || doc.MimeType == "image/jpeg",
		"expected png or jpeg, got %q", doc.MimeType)
	assert.NotEmpty(t, doc.Source.InlineData)
}

func TestProcessAttachmentWithMetadata_ImageTooLarge_Resized(t *testing.T) {
	t.Parallel()
	bigData := encodeJPEGBytes(chat.MaxImageDimension+200, chat.MaxImageDimension+200)
	path := writeTempFile(t, ".jpg", bigData)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/jpeg"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)

	img, _, decErr := image.Decode(bytes.NewReader(doc.Source.InlineData))
	require.NoError(t, decErr)
	b := img.Bounds()
	assert.LessOrEqual(t, b.Dx(), chat.MaxImageDimension)
	assert.LessOrEqual(t, b.Dy(), chat.MaxImageDimension)
}

func TestProcessAttachmentWithMetadata_IdempotentResend(t *testing.T) {
	t.Parallel()
	// An image that is already within the dimension limits
	smallData := encodeJPEGBytes(50, 50)

	doc, resizeMeta, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "photo.jpg",
			MimeType: "image/jpeg",
			Source:   chat.DocumentSource{InlineData: smallData},
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)

	// Since it fits within the limit, it shouldn't be resized
	if assert.NotNil(t, resizeMeta) {
		assert.False(t, resizeMeta.Resized)
	}

	// FormatDimensionNote should return empty string for un-resized images
	note := chat.FormatDimensionNote(resizeMeta)
	assert.Empty(t, note, "expected no dimension note for an image that wasn't resized")
}

func TestProcessAttachmentWithMetadata_PDF_Passthrough(t *testing.T) {
	t.Parallel()
	pdfBytes := []byte("%PDF-1.4 fake pdf content for testing")
	path := writeTempFile(t, ".pdf", pdfBytes)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "application/pdf"},
	})
	require.NoError(t, err)
	assert.Equal(t, "application/pdf", doc.MimeType)
	assert.Equal(t, pdfBytes, doc.Source.InlineData)
	assert.Empty(t, doc.Source.InlineText)
}

func TestProcessAttachmentWithMetadata_BinaryFileTooLarge_Error(t *testing.T) {
	t.Parallel()
	// Sparse file: Stat.Size > MaxInlineBinarySize without allocating memory.
	path := writeTempFile(t, ".pdf", nil)
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(chat.MaxInlineBinarySize+1))
	require.NoError(t, f.Close())

	_, _, err = chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestProcessAttachmentWithMetadata_TextFile_InlineText(t *testing.T) {
	t.Parallel()
	content := "Hello, this is a text file.\nLine 2."
	path := writeTempFile(t, ".txt", []byte(content))

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "text/plain"},
	})
	require.NoError(t, err)
	assert.Empty(t, doc.Source.InlineData)
	assert.Contains(t, doc.Source.InlineText, content)
}

func TestProcessAttachmentWithMetadata_MarkdownFile_InlineText(t *testing.T) {
	t.Parallel()
	content := "# Title\n\nBody paragraph."
	path := writeTempFile(t, ".md", []byte(content))

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path},
	})
	require.NoError(t, err)
	assert.Empty(t, doc.Source.InlineData)
	assert.Contains(t, doc.Source.InlineText, content)
}

func TestProcessAttachmentWithMetadata_MissingFile_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: "/nonexistent/path/file.jpg"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot stat")
}

func TestProcessAttachmentWithMetadata_NilFile_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: nil,
	})
	require.Error(t, err)
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachmentWithMetadata — MessagePartTypeImageURL
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachmentWithMetadata_DataURI_JPEG(t *testing.T) {
	t.Parallel()
	jpegData := encodeJPEGBytes(50, 50)
	dataURI := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: dataURI},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)
	assert.True(t, doc.MimeType == "image/jpeg" || doc.MimeType == "image/png")
}

func TestProcessAttachmentWithMetadata_DataURI_PNG(t *testing.T) {
	t.Parallel()
	pngData := encodePNGBytes(50, 50, false)
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)

	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: dataURI},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)
}

func TestProcessAttachmentWithMetadata_DataURI_NonBase64_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: "data:text/plain,hello"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not base64")
}

func TestProcessAttachmentWithMetadata_RemoteURL_Error(t *testing.T) {
	t.Parallel()
	// Remote http(s):// URLs are not supported; callers must download locally.
	for _, scheme := range []string{"http://", "https://"} {
		_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
			Type:     chat.MessagePartTypeImageURL,
			ImageURL: &chat.MessageImageURL{URL: scheme + "example.com/photo.jpg"},
		})
		require.Error(t, err, "expected error for scheme %s", scheme)
		assert.Contains(t, err.Error(), "remote URLs are not supported")
	}
}

func TestProcessAttachmentWithMetadata_UnsupportedScheme_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: "ftp://example.com/image.jpg"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported image URL scheme")
}

func TestProcessAttachmentWithMetadata_NilImageURL_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: nil,
	})
	require.Error(t, err)
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachmentWithMetadata — MessagePartTypeDocument
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachmentWithMetadata_Document_WithInlineData_Passthrough(t *testing.T) {
	t.Parallel()
	pdfBytes := []byte("%PDF-1.4 test")
	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "spec.pdf",
			MimeType: "application/pdf",
			Source:   chat.DocumentSource{InlineData: pdfBytes},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, pdfBytes, doc.Source.InlineData)
	assert.Equal(t, "application/pdf", doc.MimeType)
}

func TestProcessAttachmentWithMetadata_Document_WithInlineText_Passthrough(t *testing.T) {
	t.Parallel()
	text := "# Markdown content"
	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "readme.md",
			MimeType: "text/markdown",
			Source:   chat.DocumentSource{InlineText: text},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, text, doc.Source.InlineText)
	assert.Empty(t, doc.Source.InlineData)
}

func TestProcessAttachmentWithMetadata_Document_ImageInlineData_Transcoded(t *testing.T) {
	t.Parallel()
	jpegData := encodeJPEGBytes(40, 40)
	doc, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "photo.jpg",
			MimeType: "image/jpeg",
			Source:   chat.DocumentSource{InlineData: jpegData},
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)
	assert.True(t, doc.MimeType == "image/jpeg" || doc.MimeType == "image/png")
}

func TestProcessAttachmentWithMetadata_Document_NoContent_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     "empty.md",
			MimeType: "text/markdown",
			Source:   chat.DocumentSource{},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no inline content")
}

func TestProcessAttachmentWithMetadata_Document_NilDocument_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type:     chat.MessagePartTypeDocument,
		Document: nil,
	})
	require.Error(t, err)
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachmentWithMetadata — unsupported type
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachmentWithMetadata_UnsupportedType_Error(t *testing.T) {
	t.Parallel()
	_, _, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
		Type: chat.MessagePartTypeText,
		Text: "hello",
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "unsupported")
}
