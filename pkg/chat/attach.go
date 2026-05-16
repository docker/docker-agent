package chat

// Package-level attach-time processing pipeline.
//
// The attach-time pipeline runs once when a user adds a file or image to a
// message. It produces a fully-resolved [Document] whose Source.InlineData or
// Source.InlineText is populated. Provider-level convertDocument functions then
// consume the Document at inference time — they never perform I/O or resizing.
//
//	producer (app.go / runner.go)
//	  └─ ProcessAttachment(ctx, part) → Document
//	       └─ session.UserMessage(…, MessagePart{Type: MessagePartTypeDocument, Document: &doc})
//	            └─ per-provider convertDocument(ctx, doc, modelID) → wire format

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaxInlineBinarySize is the maximum byte size of a binary file (PDF, image,
	// etc.) that will be read into memory for inline attachment.
	MaxInlineBinarySize = 20 * 1024 * 1024 // 20 MB
)

// ProcessAttachment converts a raw [MessagePart] into a [Document] with fully
// resolved Source.InlineData or Source.InlineText. It is called once when a
// message is assembled — never at inference time.
//
// Supported input types:
//
//   - [MessagePartTypeFile]: reads the file from the local filesystem, detects
//     its MIME type, and either inlines text content (text/* files) or reads
//     binary bytes (images are transcoded+resized via [ResizeImage]; PDFs and
//     other supported types are read verbatim).
//
//   - [MessagePartTypeImageURL]: handles data: URIs (decoded inline). Remote
//     http(s):// URLs are not supported; callers should download the file
//     locally first and pass it as a [MessagePartTypeFile] instead.
//
//   - [MessagePartTypeDocument]: if Source.InlineData or Source.InlineText is
//     already set, the document is returned as-is after applying image
//     transcoding to any image/* InlineData. A Document with no inline content
//     is an error.
func ProcessAttachment(_ context.Context, part MessagePart) (Document, error) {
	doc, _, err := ProcessAttachmentWithMetadata(part)
	return doc, err
}

// ProcessAttachmentWithMetadata is like [ProcessAttachment] but also returns
// the [ImageResizeResult] when the attachment was an image that went through
// [ResizeImage]. The metadata is nil for non-image attachments.
//
// Callers that need to emit a dimension note (for model coordinate-mapping)
// should use this variant and call [FormatDimensionNote] on the returned
// metadata.
func ProcessAttachmentWithMetadata(part MessagePart) (Document, *ImageResizeResult, error) {
	switch part.Type {
	case MessagePartTypeFile:
		return processFilePart(part)
	case MessagePartTypeImageURL:
		return processImageURLPart(part)
	case MessagePartTypeDocument:
		return processDocumentPart(part)
	default:
		return Document{}, nil, fmt.Errorf("ProcessAttachment: unsupported part type %q", part.Type)
	}
}

// processFilePart handles MessagePartTypeFile: reads from disk, detects MIME,
// routes to text-inline or binary-inline as appropriate.
func processFilePart(part MessagePart) (Document, *ImageResizeResult, error) {
	if part.File == nil {
		return Document{}, nil, errors.New("ProcessAttachment: file part has nil File field")
	}
	absPath := part.File.Path
	name := filepath.Base(absPath)

	fi, err := os.Stat(absPath)
	if err != nil {
		return Document{}, nil, fmt.Errorf("ProcessAttachment: cannot stat %q: %w", absPath, err)
	}
	if !fi.Mode().IsRegular() {
		return Document{}, nil, fmt.Errorf("ProcessAttachment: %q is not a regular file", absPath)
	}

	mimeType := DetectMimeType(absPath)

	// Route by MIME type. Note: MIME-type check must precede IsTextFile because
	// some binary formats (e.g. PDF) may pass the text heuristic when the file
	// content happens to be printable ASCII.
	switch {
	case IsImageMimeType(mimeType):
		if fi.Size() > MaxInlineBinarySize {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: image file %q too large to inline (%d bytes, max %d)", absPath, fi.Size(), MaxInlineBinarySize)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: read image %q: %w", absPath, err)
		}
		return transcodeImageWithMeta(name, data, mimeType)

	case mimeType == "application/pdf" || (IsSupportedMimeType(mimeType) && !IsTextFile(absPath)):
		// PDF and other supported binary types — read verbatim.
		// The !IsTextFile guard ensures that binary formats whose extension
		// is unknown but content is ASCII-printable are not incorrectly inlined.
		if fi.Size() > MaxInlineBinarySize {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: binary file %q too large to inline (%d bytes, max %d)", absPath, fi.Size(), MaxInlineBinarySize)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: read binary file %q: %w", absPath, err)
		}
		return Document{
			Name:     name,
			MimeType: mimeType,
			Size:     int64(len(data)),
			Source:   DocumentSource{InlineData: data},
		}, nil, nil

	case IsTextFile(absPath):
		if fi.Size() > MaxInlineFileSize {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: text file %q too large to inline (%d bytes, max %d)", absPath, fi.Size(), MaxInlineFileSize)
		}
		content, err := ReadFileForInline(absPath)
		if err != nil {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: read text file %q: %w", absPath, err)
		}
		return Document{
			Name:     name,
			MimeType: mimeType,
			Size:     fi.Size(),
			Source:   DocumentSource{InlineText: content},
		}, nil, nil

	default:
		// Unknown binary — read verbatim and let modelinfo gate it at inference time.
		if fi.Size() > MaxInlineBinarySize {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: file %q too large to inline (%d bytes, max %d)", absPath, fi.Size(), MaxInlineBinarySize)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: read file %q: %w", absPath, err)
		}
		return Document{
			Name:     name,
			MimeType: mimeType,
			Size:     int64(len(data)),
			Source:   DocumentSource{InlineData: data},
		}, nil, nil
	}
}

// processImageURLPart handles MessagePartTypeImageURL.
// Only data: URIs are supported; remote http(s):// URLs are rejected.
// Callers with a remote URL should download the file locally first and
// pass it as a MessagePartTypeFile instead.
func processImageURLPart(part MessagePart) (Document, *ImageResizeResult, error) {
	if part.ImageURL == nil {
		return Document{}, nil, errors.New("ProcessAttachment: image-url part has nil ImageURL field")
	}
	rawURL := part.ImageURL.URL

	switch {
	case strings.HasPrefix(rawURL, "data:"):
		mimeType, data, err := parseDataURI(rawURL)
		if err != nil {
			return Document{}, nil, fmt.Errorf("ProcessAttachment: parse data URI: %w", err)
		}
		// When content detection returns an image type, prefer it over the
		// declared MIME. (Only image types are trusted from the sniffer.)
		if detected := DetectMimeTypeByContent(data); IsImageMimeType(detected) {
			mimeType = detected
		}
		return transcodeImageWithMeta("image", data, mimeType)

	case strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://"):
		return Document{}, nil, errors.New("attachment: remote URLs are not supported; download the file locally first")

	default:
		return Document{}, nil, fmt.Errorf("attachment: unsupported image URL scheme: %q", rawURL)
	}
}

// processDocumentPart handles MessagePartTypeDocument.
// Images with InlineData are transcoded; other already-resolved documents pass through.
func processDocumentPart(part MessagePart) (Document, *ImageResizeResult, error) {
	if part.Document == nil {
		return Document{}, nil, errors.New("ProcessAttachment: document part has nil Document field")
	}
	doc := *part.Document

	if len(doc.Source.InlineData) > 0 {
		if IsImageMimeType(doc.MimeType) {
			return transcodeImageWithMeta(doc.Name, doc.Source.InlineData, doc.MimeType)
		}
		return doc, nil, nil
	}

	if doc.Source.InlineText != "" {
		return doc, nil, nil
	}

	return Document{}, nil, fmt.Errorf("ProcessAttachment: document %q has no inline content (InlineData and InlineText are both empty)", doc.Name)
}

// transcodeImageWithMeta runs bytes through ResizeImage to normalise the image
// to JPEG or PNG within provider limits, then wraps the result in a Document.
// Returns the [ImageResizeResult] so callers can emit dimension notes.
func transcodeImageWithMeta(name string, data []byte, mimeType string) (Document, *ImageResizeResult, error) {
	result, err := ResizeImage(data, mimeType)
	if err != nil {
		return Document{}, nil, fmt.Errorf("ProcessAttachment: transcode image %q: %w", name, err)
	}
	return Document{
		Name:     name,
		MimeType: result.MimeType,
		Size:     int64(len(result.Data)),
		Source:   DocumentSource{InlineData: result.Data},
	}, result, nil
}

// parseDataURI parses a data URI of the form "data:<mime>;base64,<payload>".
// Returns the MIME type and decoded bytes.
func parseDataURI(uri string) (mimeType string, data []byte, err error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return "", nil, errors.New("not a data URI")
	}

	header, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, errors.New("data URI missing comma separator")
	}

	// Header is "<mime>[;charset=…];base64" or "<mime>" (plain text, unsupported here).
	if !strings.HasSuffix(header, ";base64") {
		return "", nil, errors.New("data URI is not base64-encoded (only base64 data URIs are supported)")
	}
	mimeType = strings.TrimSuffix(header, ";base64")

	// Strip any charset parameter (e.g. "image/png;charset=utf-8;base64" → "image/png").
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	data, err = base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, fmt.Errorf("base64 decode: %w", err)
	}
	return mimeType, data, nil
}
