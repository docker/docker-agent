package editor

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxAttachmentPreviewBytes = 20 << 20

// AttachmentPreview keeps encoded image bytes out of the terminal text path.
type AttachmentPreview struct {
	Title     string
	Content   string
	MIME      string
	ImageData []byte
	IsImage   bool
}

func loadAttachmentPreview(title, path string) AttachmentPreview {
	preview := AttachmentPreview{Title: safePreviewLabel(title)}
	file, err := os.Open(path)
	if err != nil {
		preview.Content = "Unable to read attachment: " + safePreviewLabel(err.Error())
		return preview
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentPreviewBytes+1))
	if err != nil {
		preview.Content = "Unable to read attachment: " + safePreviewLabel(err.Error())
		return preview
	}
	if len(data) > maxAttachmentPreviewBytes {
		preview.Content = "Attachment preview unavailable: file exceeds the 20 MiB preview limit."
		return preview
	}
	preview.MIME = http.DetectContentType(data)
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		preview.IsImage = true
		preview.ImageData = data
		return preview
	}
	if !utf8.Valid(data) || strings.ContainsFunc(string(data), func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	}) {
		preview.Content = "Attachment preview unavailable: binary or terminal-control data (" + preview.MIME + ")."
		return preview
	}
	preview.Content = strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	return preview
}

func safePreviewLabel(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, text)
}
