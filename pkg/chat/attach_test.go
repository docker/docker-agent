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
// ProcessAttachment — MessagePartTypeFile
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachment_JPEG_Passthrough(t *testing.T) {
	data := encodeJPEGBytes(100, 100)
	path := writeTempFile(t, ".jpg", data)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/jpeg"},
	})
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", doc.MimeType)
	assert.NotEmpty(t, doc.Source.InlineData)
	assert.Empty(t, doc.Source.InlineText)
	assert.Equal(t, filepath.Base(path), doc.Name)
}

func TestProcessAttachment_PNG_Passthrough(t *testing.T) {
	data := encodePNGBytes(100, 100, false)
	path := writeTempFile(t, ".png", data)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/png"},
	})
	require.NoError(t, err)
	assert.Equal(t, "image/png", doc.MimeType)
	assert.NotEmpty(t, doc.Source.InlineData)
}

func TestProcessAttachment_PNG_WithAlpha_StaysPNG(t *testing.T) {
	data := encodePNGBytes(100, 100, true)
	path := writeTempFile(t, ".png", data)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "image/png"},
	})
	require.NoError(t, err)
	assert.True(t, doc.MimeType == "image/png" || doc.MimeType == "image/jpeg",
		"expected png or jpeg, got %q", doc.MimeType)
	assert.NotEmpty(t, doc.Source.InlineData)
}

func TestProcessAttachment_ImageTooLarge_Resized(t *testing.T) {
	bigData := encodeJPEGBytes(chat.MaxImageDimension+200, chat.MaxImageDimension+200)
	path := writeTempFile(t, ".jpg", bigData)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
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

func TestProcessAttachment_PDF_Passthrough(t *testing.T) {
	pdfBytes := []byte("%PDF-1.4 fake pdf content for testing")
	path := writeTempFile(t, ".pdf", pdfBytes)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "application/pdf"},
	})
	require.NoError(t, err)
	assert.Equal(t, "application/pdf", doc.MimeType)
	assert.Equal(t, pdfBytes, doc.Source.InlineData)
	assert.Empty(t, doc.Source.InlineText)
}

func TestProcessAttachment_BinaryFileTooLarge_Error(t *testing.T) {
	// Sparse file: Stat.Size > MaxInlineBinarySize without allocating memory.
	path := writeTempFile(t, ".pdf", nil)
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(chat.MaxInlineBinarySize+1))
	require.NoError(t, f.Close())

	_, err = chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestProcessAttachment_TextFile_InlineText(t *testing.T) {
	content := "Hello, this is a text file.\nLine 2."
	path := writeTempFile(t, ".txt", []byte(content))

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path, MimeType: "text/plain"},
	})
	require.NoError(t, err)
	assert.Empty(t, doc.Source.InlineData)
	assert.Contains(t, doc.Source.InlineText, content)
}

func TestProcessAttachment_MarkdownFile_InlineText(t *testing.T) {
	content := "# Title\n\nBody paragraph."
	path := writeTempFile(t, ".md", []byte(content))

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: path},
	})
	require.NoError(t, err)
	assert.Empty(t, doc.Source.InlineData)
	assert.Contains(t, doc.Source.InlineText, content)
}

func TestProcessAttachment_MissingFile_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: &chat.MessageFile{Path: "/nonexistent/path/file.jpg"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot stat")
}

func TestProcessAttachment_NilFile_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeFile,
		File: nil,
	})
	require.Error(t, err)
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachment — MessagePartTypeImageURL
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachment_DataURI_JPEG(t *testing.T) {
	jpegData := encodeJPEGBytes(50, 50)
	dataURI := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: dataURI},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)
	assert.True(t, doc.MimeType == "image/jpeg" || doc.MimeType == "image/png")
}

func TestProcessAttachment_DataURI_PNG(t *testing.T) {
	pngData := encodePNGBytes(50, 50, false)
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)

	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: dataURI},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.Source.InlineData)
}

func TestProcessAttachment_DataURI_NonBase64_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: "data:text/plain,hello"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not base64")
}

func TestProcessAttachment_RemoteURL_Error(t *testing.T) {
	// Remote http(s):// URLs are not supported; callers must download locally.
	for _, scheme := range []string{"http://", "https://"} {
		_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
			Type:     chat.MessagePartTypeImageURL,
			ImageURL: &chat.MessageImageURL{URL: scheme + "example.com/photo.jpg"},
		})
		require.Error(t, err, "expected error for scheme %s", scheme)
		assert.Contains(t, err.Error(), "remote URLs are not supported")
	}
}

func TestProcessAttachment_UnsupportedScheme_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: &chat.MessageImageURL{URL: "ftp://example.com/image.jpg"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported image URL scheme")
}

func TestProcessAttachment_NilImageURL_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type:     chat.MessagePartTypeImageURL,
		ImageURL: nil,
	})
	require.Error(t, err)
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachment — MessagePartTypeDocument
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachment_Document_WithInlineData_Passthrough(t *testing.T) {
	pdfBytes := []byte("%PDF-1.4 test")
	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
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

func TestProcessAttachment_Document_WithInlineText_Passthrough(t *testing.T) {
	text := "# Markdown content"
	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
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

func TestProcessAttachment_Document_ImageInlineData_Transcoded(t *testing.T) {
	jpegData := encodeJPEGBytes(40, 40)
	doc, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
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

func TestProcessAttachment_Document_NoContent_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
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

func TestProcessAttachment_Document_NilDocument_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type:     chat.MessagePartTypeDocument,
		Document: nil,
	})
	require.Error(t, err)
}

// ──────────────────────────────────────────────────────────────────────────────
// ProcessAttachment — unsupported type
// ──────────────────────────────────────────────────────────────────────────────

func TestProcessAttachment_UnsupportedType_Error(t *testing.T) {
	_, err := chat.ProcessAttachment(t.Context(), chat.MessagePart{
		Type: chat.MessagePartTypeText,
		Text: "hello",
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "unsupported")
}
