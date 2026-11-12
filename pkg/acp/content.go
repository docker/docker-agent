package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"mime"
	"net/url"
	urlpath "path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func (a *Agent) buildUserMessage(ctx context.Context, sessionID string, prompt []acp.ContentBlock) *session.Message {
	var (
		parts          []string
		multiContent   []chat.MessagePart
		hasRichContent bool
	)

	appendText := func(text string) {
		if text == "" {
			return
		}
		parts = append(parts, text)
		multiContent = append(multiContent, chat.MessagePart{Type: chat.MessagePartTypeText, Text: text})
	}

	appendDocument := func(name, mimeType string, source chat.DocumentSource) {
		doc, note, err := promptDocument(name, mimeType, source)
		if err != nil {
			appendText(fmt.Sprintf("[Attachment: %s (content unavailable)]", name))
			return
		}
		hasRichContent = true
		multiContent = append(multiContent, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: doc})
		appendText(note)
	}

	for _, content := range prompt {
		switch {
		case content.Text != nil:
			appendText(content.Text.Text)

		case content.ResourceLink != nil:
			rl := content.ResourceLink
			slog.DebugContext(ctx, "Processing resource link", "uri", rl.Uri, "name", rl.Name)

			if fileContent, ok := a.readResourceLink(ctx, sessionID, rl); ok {
				appendDocument(resourceLinkName(rl), stringOrDefault(rl.MimeType, "text/plain"), chat.DocumentSource{InlineText: fileContent})
			} else {
				appendText(fmt.Sprintf("\n[Referenced file: %s (content unavailable)]\n", resourceLinkName(rl)))
			}

		case content.Resource != nil:
			res := content.Resource.Resource
			if text := res.TextResourceContents; text != nil {
				name := resourceLinkName(&acp.ContentBlockResourceLink{Uri: text.Uri})
				appendDocument(name, stringOrDefault(text.MimeType, "text/plain"), chat.DocumentSource{InlineText: text.Text})
			} else if blob := res.BlobResourceContents; blob != nil {
				name := resourceLinkName(&acp.ContentBlockResourceLink{Uri: blob.Uri})
				data, err := decodePromptData(blob.Blob)
				if err != nil {
					appendText(fmt.Sprintf("[Attachment: %s (content unavailable)]", name))
					continue
				}
				appendDocument(name, stringOrDefault(blob.MimeType, ""), chat.DocumentSource{InlineData: data})
			}

		case content.Image != nil:
			mediaType, _, err := mime.ParseMediaType(content.Image.MimeType)
			if err != nil || !strings.HasPrefix(mediaType, "image/") {
				appendText("[Attachment: image (content unavailable)]")
				continue
			}
			data, err := decodePromptData(content.Image.Data)
			if err != nil {
				appendText("[Attachment: image (content unavailable)]")
				continue
			}
			appendDocument("image", content.Image.MimeType, chat.DocumentSource{InlineData: data})

		case content.Audio != nil:
			mimeType, ok := promptAudioMIME(content.Audio.MimeType)
			if !ok {
				appendText("[Attachment: audio (content unavailable)]")
				continue
			}
			data, err := decodePromptData(content.Audio.Data)
			if err != nil || len(data) == 0 {
				appendText("[Attachment: audio (content unavailable)]")
				continue
			}
			hasRichContent = true
			multiContent = append(multiContent, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "audio", MimeType: mimeType, Size: int64(len(data)), Source: chat.DocumentSource{InlineData: data},
			}})
			// Keep audio-only turns meaningful when a model/provider drops the bytes.
			appendText(promptAudioNote)
		}
	}

	content := strings.Join(parts, "")
	if !hasRichContent {
		return session.UserMessage(content)
	}
	return session.UserMessage(content, multiContent...)
}

// readResourceLink attempts to read a text file referenced by an ACP resource link.
func (a *Agent) readResourceLink(ctx context.Context, sessionID string, rl *acp.ContentBlockResourceLink) (string, bool) {
	if !a.supportsClientReadTextFile() {
		slog.DebugContext(ctx, "ACP client does not support reading resource links")
		return "", false
	}

	path, ok := resourceLinkPath(rl.Uri)
	if !ok {
		slog.DebugContext(ctx, "Unsupported ACP resource link URI", "uri", rl.Uri)
		return "", false
	}

	resolvedPath, err := a.resolveSessionPath(sessionID, path)
	if err != nil {
		slog.WarnContext(ctx, "Blocked unsafe file resource link", "path", path, "error", err)
		return "", false
	}

	if ctx.Err() != nil {
		return "", false
	}
	resp, err := a.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
	})
	if err != nil {
		slog.DebugContext(ctx, "Failed to read resource link", "path", resolvedPath, "error", err)
		return "", false
	}

	return resp.Content, true
}

func resourceLinkName(rl *acp.ContentBlockResourceLink) string {
	if name := chat.SanitizeDisplayName(rl.Name); name != "" {
		return name
	}
	// Display names do not require a URI to resolve on the agent's host OS.
	if uri, err := url.Parse(rl.Uri); err == nil && uri.Scheme == "file" && uri.User == nil && (uri.Host == "" || uri.Host == "localhost") {
		if base := chat.SanitizeDisplayName(urlpath.Base(uri.Path)); base != "" && base != "." && base != "/" {
			return base
		}
	}
	if path, ok := resourceLinkPath(rl.Uri); ok {
		if base := chat.SanitizeDisplayName(filepath.Base(path)); base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return "resource"
}

func resourceLinkPath(rawURI string) (string, bool) {
	if rawURI == "" || strings.ContainsRune(rawURI, 0) {
		return "", false
	}
	// Native paths are not URL-escaped, including literal percent signs.
	if filepath.IsAbs(rawURI) {
		return rawURI, !strings.HasPrefix(filepath.ToSlash(rawURI), "//")
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return rawURI, !strings.Contains(rawURI, ":") && !strings.HasPrefix(filepath.ToSlash(rawURI), "//")
	}
	if u.Scheme == "" {
		return rawURI, u.Host == ""
	}
	if u.Scheme != "file" || u.Opaque != "" || u.User != nil || (u.Host != "" && u.Host != "localhost") {
		return "", false
	}
	// url.Parse has already decoded Path; a second unescape changes literal filenames.
	path := u.Path
	if strings.ContainsRune(path, 0) || strings.HasPrefix(path, "//") {
		return "", false
	}
	if filepath.Separator == '\\' {
		if len(path) < 4 || path[0] != '/' || !isDriveLetter(path[1]) || path[2:4] != ":/" {
			return "", false
		}
		path = path[1:]
	}
	path = filepath.FromSlash(path)
	return path, filepath.IsAbs(path)
}

func isDriveLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func stringOrDefault(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// Prompt images are decoded locally; bound pixel allocation as well as encoded bytes.
const maxPromptImagePixels = 16_000_000

func decodePromptData(encoded string) ([]byte, error) {
	if len(encoded) > base64.StdEncoding.EncodedLen(chat.MaxInlineBinarySize) {
		return nil, errors.New("attachment too large")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) > chat.MaxInlineBinarySize {
		return nil, errors.New("invalid attachment data")
	}
	return data, nil
}

func promptDocument(name, mimeType string, source chat.DocumentSource) (*chat.Document, string, error) {
	if mimeType == "" {
		if source.InlineData == nil {
			mimeType = "text/plain"
		} else {
			mimeType = chat.DetectMimeTypeByContent(source.InlineData)
		}
	}
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return nil, "", err
	}
	if len(source.InlineData) > chat.MaxInlineBinarySize {
		return nil, "", errors.New("attachment too large")
	}
	if source.InlineData != nil && (strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || mediaType == "application/xml" || strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")) {
		if !utf8.Valid(source.InlineData) {
			return nil, "", errors.New("invalid text attachment")
		}
		source.InlineText = string(source.InlineData)
		source.InlineData = nil
	}
	if source.InlineData == nil && (len(source.InlineText) > chat.MaxInlineFileSize || !utf8.ValidString(source.InlineText)) {
		return nil, "", errors.New("invalid text attachment")
	}
	doc := &chat.Document{Name: name, MimeType: mediaType, Source: source, Size: int64(len(source.InlineText) + len(source.InlineData))}
	if source.InlineData != nil && strings.HasPrefix(mediaType, "image/") {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(source.InlineData))
		if err != nil {
			return nil, "", err
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPromptImagePixels/cfg.Height {
			return nil, "", errors.New("image dimensions too large")
		}
		doc.MimeType = chat.DetectMimeTypeByContent(source.InlineData)
		processed, resized, err := chat.ProcessAttachmentWithMetadata(chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: doc})
		if err != nil {
			return nil, "", err
		}
		if resized != nil {
			return &processed, chat.FormatDimensionNote(resized), nil
		}
		return &processed, "", nil
	}
	if len(source.InlineData) == 0 && source.InlineText == "" {
		return doc, fmt.Sprintf("[Attachment: %s (empty)]", name), nil
	}
	return doc, "", nil
}

const promptAudioNote = "[Audio attachment supplied; not a transcript. Audio access depends on the selected model/provider.]"

func promptAudioMIME(value string) (string, bool) {
	if len(value) > 256 {
		return "", false
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || !strings.HasPrefix(mediaType, "audio/") || strings.Contains(mediaType, "*") {
		return "", false
	}
	canonical := mime.FormatMediaType(mediaType, params)
	return canonical, canonical != "" && len(canonical) <= 256
}
