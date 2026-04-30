package gemini

// attachments.go — Phase 1 attachment support for the Gemini provider.

import (
	"context"
	"log/slog"
	"sort"

	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
)

// capabilityTable is the Gemini Phase 1 capability table.
// Gemini only supports B64 (inline binary) and TXT; no URL delivery for documents.
// Phase 1: no Files API.
var capabilityTable = attachment.CapabilityTable{
	"image/jpeg":      {B64: true},
	"image/png":       {B64: true},
	"image/gif":       {B64: true},
	"image/webp":      {B64: true},
	"image/heic":      {B64: true},
	"image/heif":      {B64: true},
	"application/pdf": {B64: true},
	"text/plain":      {TXT: true},
	"text/markdown":   {TXT: true},
	"text/html":       {TXT: true},
	"text/csv":        {TXT: true},
	"video/mp4":       {B64: true},
	"video/mov":       {B64: true},
	"video/webm":      {B64: true},
	"video/avi":       {B64: true},
	"video/mkv":       {B64: true},
	"audio/wav":       {B64: true},
	"audio/mp3":       {B64: true},
	"audio/flac":      {B64: true},
	"audio/aac":       {B64: true},
	"audio/ogg":       {B64: true},
	"audio/aiff":      {B64: true},
}

// SupportedMIMETypes implements attachment.Advisor.
// Returns every MIME type that has at least one capability, sorted for determinism.
func (c *Client) SupportedMIMETypes() []string {
	mimes := make([]string, 0, len(capabilityTable))
	for mime, cap := range capabilityTable {
		if cap.TXT || cap.B64 || cap.URL {
			mimes = append(mimes, mime)
		}
	}
	sort.Strings(mimes)
	return mimes
}

// convertDocument converts a chat.Document into zero or more *genai.Part values.
//
// On StrategyDrop the attachment is silently skipped (a Warn is logged).
func (c *Client) convertDocument(ctx context.Context, doc chat.Document) ([]*genai.Part, error) {
	strategy, reason := attachment.Decide(doc, capabilityTable)

	switch strategy {
	case attachment.StrategyB64:
		return convertDocViaB64(doc), nil

	case attachment.StrategyTXT:
		return convertDocViaTXT(doc), nil

	case attachment.StrategyFetchAsB64:
		slog.Warn("attachment: fetching URL as base64", "name", doc.Name, "reason", reason)
		return convertDocViaFetchAsB64(ctx, doc)

	case attachment.StrategyFetchAsTXT:
		slog.Warn("attachment: fetching URL as text", "name", doc.Name, "reason", reason)
		return convertDocViaFetchAsTXT(ctx, doc)

	default: // StrategyDrop (and StrategyURL — not in Gemini table)
		slog.Warn("attachment: dropping document", "name", doc.Name, "mime", doc.MimeType, "reason", reason)
		return nil, nil
	}
}

// convertDocViaB64 creates a Blob part from inline binary data.
func convertDocViaB64(doc chat.Document) []*genai.Part {
	return []*genai.Part{
		genai.NewPartFromBytes(doc.Source.InlineData, doc.MimeType),
	}
}

// convertDocViaTXT wraps inline text in an XML envelope and returns a text part.
func convertDocViaTXT(doc chat.Document) []*genai.Part {
	envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
	return []*genai.Part{
		genai.NewPartFromText(envelope),
	}
}

// convertDocViaFetchAsB64 fetches the URL and returns its bytes as a Blob part.
func convertDocViaFetchAsB64(ctx context.Context, doc chat.Document) ([]*genai.Part, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for b64", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineData = data
	return convertDocViaB64(fetched), nil
}

// convertDocViaFetchAsTXT fetches the URL and returns its content as a text envelope.
func convertDocViaFetchAsTXT(ctx context.Context, doc chat.Document) ([]*genai.Part, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for text", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineText = string(data)
	return convertDocViaTXT(fetched), nil
}
