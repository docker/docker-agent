package anthropic

// attachments.go — Phase 1 attachment support for the Anthropic provider.
//
// convertDocument dispatches via attachment.Decide and returns zero or more
// anthropic.ContentBlockParamUnion values ready for inclusion in a user message.
// It handles both the standard and Beta API paths identically: images become
// image blocks, text documents become text blocks (wrapped with TXTEnvelope),
// and PDFs become document blocks with base64 source.
//
// Wire-up: convertUserMultiContent and convertBetaUserMultiContent both call
// convertDocument for chat.MessagePartTypeDocument parts.

import (
	"context"
	"encoding/base64"
	"log/slog"
	"sort"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
)

// capabilityTable is the Anthropic Phase 1 capability table.
// Phase 1: TXT / B64 / URL only — no Files API.
var capabilityTable = attachment.CapabilityTable{
	"image/jpeg":      {B64: true, URL: true},
	"image/png":       {B64: true, URL: true},
	"image/gif":       {B64: true, URL: true},
	"image/webp":      {B64: true, URL: true},
	"application/pdf": {B64: true},
	"text/plain":      {TXT: true},
	"text/markdown":   {TXT: true},
	"text/html":       {TXT: true},
	"text/csv":        {TXT: true},
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

// convertDocument converts a chat.Document into zero or more
// anthropic.ContentBlockParamUnion values.
//
// On StrategyDrop the attachment is silently skipped (a Warn is logged).
func (c *Client) convertDocument(
	ctx context.Context,
	doc chat.Document,
) ([]anthropic.ContentBlockParamUnion, error) {
	strategy, reason := attachment.Decide(doc, capabilityTable)

	switch strategy {
	case attachment.StrategyURL:
		return convertDocViaURL(doc), nil

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

	default: // StrategyDrop
		slog.Warn("attachment: dropping document", "name", doc.Name, "mime", doc.MimeType, "reason", reason)
		return nil, nil
	}
}

// convertDocViaURL builds a content block that passes the URL directly.
// NOTE: this function assumes URL is only set for image MIMEs per the capability
// table. If application/pdf:{URL:true} is ever added, use
// NewDocumentBlock(URLPDFSourceParam{URL: doc.Source.URL}) instead of
// NewImageBlock — the Anthropic API rejects an image block for a PDF URL.
func convertDocViaURL(doc chat.Document) []anthropic.ContentBlockParamUnion {
	return []anthropic.ContentBlockParamUnion{
		anthropic.NewImageBlock(anthropic.URLImageSourceParam{
			URL: doc.Source.URL,
		}),
	}
}

// convertDocViaB64 encodes inline binary as a base64 block.
// Images use image blocks; PDFs use document blocks.
func convertDocViaB64(doc chat.Document) []anthropic.ContentBlockParamUnion {
	encoded := base64.StdEncoding.EncodeToString(doc.Source.InlineData)

	if IsImageMime(doc.MimeType) {
		return []anthropic.ContentBlockParamUnion{
			anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{
				Data:      encoded,
				MediaType: anthropic.Base64ImageSourceMediaType(doc.MimeType),
			}),
		}
	}

	// PDF or other binary document
	return []anthropic.ContentBlockParamUnion{
		anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
			Data: encoded,
		}),
	}
}

// convertDocViaTXT wraps inline text in an XML envelope and returns a text block.
func convertDocViaTXT(doc chat.Document) []anthropic.ContentBlockParamUnion {
	envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
	return []anthropic.ContentBlockParamUnion{
		anthropic.NewTextBlock(envelope),
	}
}

// convertDocViaFetchAsB64 fetches the URL and returns the bytes as a base64 block.
func convertDocViaFetchAsB64(ctx context.Context, doc chat.Document) ([]anthropic.ContentBlockParamUnion, error) {
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
func convertDocViaFetchAsTXT(ctx context.Context, doc chat.Document) ([]anthropic.ContentBlockParamUnion, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for text", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineText = string(data)
	return convertDocViaTXT(fetched), nil
}

// convertBetaDocument converts a chat.Document into zero or more
// anthropic.BetaContentBlockParamUnion values (Beta API).
//
// On StrategyDrop the attachment is silently skipped (a Warn is logged).
func (c *Client) convertBetaDocument(
	ctx context.Context,
	doc chat.Document,
) ([]anthropic.BetaContentBlockParamUnion, error) {
	strategy, reason := attachment.Decide(doc, capabilityTable)

	switch strategy {
	case attachment.StrategyURL:
		return convertBetaDocViaURL(doc), nil

	case attachment.StrategyB64:
		return convertBetaDocViaB64(doc), nil

	case attachment.StrategyTXT:
		return convertBetaDocViaTXT(doc), nil

	case attachment.StrategyFetchAsB64:
		slog.Warn("attachment: fetching URL as base64 (beta)", "name", doc.Name, "reason", reason)
		return convertBetaDocViaFetchAsB64(ctx, doc)

	case attachment.StrategyFetchAsTXT:
		slog.Warn("attachment: fetching URL as text (beta)", "name", doc.Name, "reason", reason)
		return convertBetaDocViaFetchAsTXT(ctx, doc)

	default: // StrategyDrop
		slog.Warn("attachment: dropping document (beta)", "name", doc.Name, "mime", doc.MimeType, "reason", reason)
		return nil, nil
	}
}

func convertBetaDocViaURL(doc chat.Document) []anthropic.BetaContentBlockParamUnion {
	return []anthropic.BetaContentBlockParamUnion{
		{
			OfImage: &anthropic.BetaImageBlockParam{
				Source: anthropic.BetaImageBlockParamSourceUnion{
					OfURL: &anthropic.BetaURLImageSourceParam{URL: doc.Source.URL},
				},
			},
		},
	}
}

func convertBetaDocViaB64(doc chat.Document) []anthropic.BetaContentBlockParamUnion {
	encoded := base64.StdEncoding.EncodeToString(doc.Source.InlineData)

	if IsImageMime(doc.MimeType) {
		return []anthropic.BetaContentBlockParamUnion{
			{
				OfImage: &anthropic.BetaImageBlockParam{
					Source: anthropic.BetaImageBlockParamSourceUnion{
						OfBase64: &anthropic.BetaBase64ImageSourceParam{
							Data:      encoded,
							MediaType: anthropic.BetaBase64ImageSourceMediaType(doc.MimeType),
						},
					},
				},
			},
		}
	}

	// PDF document
	return []anthropic.BetaContentBlockParamUnion{
		{
			OfDocument: &anthropic.BetaRequestDocumentBlockParam{
				Source: anthropic.BetaRequestDocumentBlockSourceUnionParam{
					OfBase64: &anthropic.BetaBase64PDFSourceParam{
						Data: encoded,
					},
				},
			},
		},
	}
}

func convertBetaDocViaTXT(doc chat.Document) []anthropic.BetaContentBlockParamUnion {
	envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
	return []anthropic.BetaContentBlockParamUnion{
		{OfText: &anthropic.BetaTextBlockParam{Text: envelope}},
	}
}

func convertBetaDocViaFetchAsB64(ctx context.Context, doc chat.Document) ([]anthropic.BetaContentBlockParamUnion, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for b64 (beta)", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineData = data
	return convertBetaDocViaB64(fetched), nil
}

func convertBetaDocViaFetchAsTXT(ctx context.Context, doc chat.Document) ([]anthropic.BetaContentBlockParamUnion, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for text (beta)", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineText = string(data)
	return convertBetaDocViaTXT(fetched), nil
}
