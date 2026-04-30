package openai

import (
	"context"
	"encoding/base64"
	"log/slog"
	"sort"

	"github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
)

// Shared capability tables referenced by multiple providers (reduces drift).

// capabilityTableImageURLB64 is the image-URL+B64 table for OpenAI-compatible
// providers that support image URLs: openai, azure, requesty.
// Note: application/pdf is absent in Phase 1 — these providers require the
// Files API for PDFs (Phase 2).
var capabilityTableImageURLB64 = attachment.CapabilityTable{
	"image/jpeg":    {B64: true, URL: true},
	"image/png":     {B64: true, URL: true},
	"image/gif":     {B64: true, URL: true},
	"image/webp":    {B64: true, URL: true},
	"text/plain":    {TXT: true},
	"text/markdown": {TXT: true},
	"text/html":     {TXT: true},
	"text/csv":      {TXT: true},
}

// capabilityTableImageB64Only is the image-B64-only table for providers that
// do not support image URLs: ollama, nebius, minimax, github-copilot.
var capabilityTableImageB64Only = attachment.CapabilityTable{
	"image/jpeg":    {B64: true},
	"image/png":     {B64: true},
	"text/plain":    {TXT: true},
	"text/markdown": {TXT: true},
	"text/html":     {TXT: true},
	"text/csv":      {TXT: true},
}

// capabilityTableByProvider maps provider names (from ModelConfig.Provider) to
// their MIME-type capability tables.
//
// Phase 1: TXT / B64 / URL only — no Files API.
var capabilityTableByProvider = map[string]attachment.CapabilityTable{
	// OpenAI Chat Completions / Responses API.
	"openai": capabilityTableImageURLB64,
	// Azure OpenAI — same capabilities as OpenAI (Phase 1).
	"azure": capabilityTableImageURLB64,
	// Requesty — acts as an OpenAI-compatible proxy (Phase 1).
	"requesty": capabilityTableImageURLB64,

	// Mistral — images via B64 or URL; no gif.
	// application/pdf: Mistral requires document_url content type (not image_url) — Phase 2.
	"mistral": {
		"image/jpeg":    {B64: true, URL: true},
		"image/png":     {B64: true, URL: true},
		"image/webp":    {B64: true, URL: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},

	// xAI (Grok) — images B64/URL; text only; no PDF.
	"xai": {
		"image/jpeg":    {B64: true, URL: true},
		"image/png":     {B64: true, URL: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},

	// Ollama, Nebius, MiniMax, GitHub Copilot — images B64 only; no PDF; no URL images.
	"ollama":         capabilityTableImageB64Only,
	"nebius":         capabilityTableImageB64Only,
	"minimax":        capabilityTableImageB64Only,
	"github-copilot": capabilityTableImageB64Only,
}

// capabilityTable returns the capability table for this client's provider,
// falling back to the openai table for unknown providers.
func (c *Client) capabilityTable() attachment.CapabilityTable {
	if table, ok := capabilityTableByProvider[c.ModelConfig.Provider]; ok {
		return table
	}
	return capabilityTableImageURLB64
}

// SupportedMIMETypes implements attachment.Advisor.
// It returns every MIME type for which this provider has at least one
// capability set (TXT, B64, or URL), sorted for determinism.
func (c *Client) SupportedMIMETypes() []string {
	table := c.capabilityTable()
	mimes := make([]string, 0, len(table))
	for mime, cap := range table {
		if cap.TXT || cap.B64 || cap.URL {
			mimes = append(mimes, mime)
		}
	}
	sort.Strings(mimes)
	return mimes
}

// convertDocument converts a chat.Document into zero or more
// openai.ChatCompletionContentPartUnionParam parts.
//
// On StrategyDrop the attachment is silently skipped (a Warn is logged).
func (c *Client) convertDocument(
	ctx context.Context,
	doc chat.Document,
) ([]openai.ChatCompletionContentPartUnionParam, error) {
	strategy, reason := attachment.Decide(doc, c.capabilityTable())

	switch strategy {
	case attachment.StrategyURL:
		return c.convertViaURL(doc), nil

	case attachment.StrategyB64:
		return c.convertViaB64(doc), nil

	case attachment.StrategyTXT:
		return c.convertViaTXT(doc), nil

	case attachment.StrategyFetchAsB64:
		slog.Warn("attachment: fetching URL as base64", "name", doc.Name, "reason", reason)
		return c.convertViaFetchAsB64(ctx, doc)

	case attachment.StrategyFetchAsTXT:
		slog.Warn("attachment: fetching URL as text", "name", doc.Name, "reason", reason)
		return c.convertViaFetchAsTXT(ctx, doc)

	default: // StrategyDrop
		slog.Warn("attachment: dropping document", "name", doc.Name, "mime", doc.MimeType, "reason", reason)
		return nil, nil
	}
}

// convertViaURL builds a part that passes the document URL directly to the model.
// Only called for image MIMEs in Phase 1 (the capability tables only grant URL
// to image types). If a future table entry adds URL to a non-image MIME, this
// function will need to branch on MIME type.
func (c *Client) convertViaURL(doc chat.Document) []openai.ChatCompletionContentPartUnionParam {
	return []openai.ChatCompletionContentPartUnionParam{
		openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL: doc.Source.URL,
		}),
	}
}

// convertViaB64 encodes inline binary as a base64 data-URL image part.
// Only called for image MIMEs in Phase 1 (PDFs use the Files API — Phase 2).
func (c *Client) convertViaB64(doc chat.Document) []openai.ChatCompletionContentPartUnionParam {
	encoded := base64.StdEncoding.EncodeToString(doc.Source.InlineData)
	dataURL := "data:" + doc.MimeType + ";base64," + encoded
	return []openai.ChatCompletionContentPartUnionParam{
		openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL: dataURL,
		}),
	}
}

// convertViaTXT wraps inline text in an XML envelope and returns it as a text part.
func (c *Client) convertViaTXT(doc chat.Document) []openai.ChatCompletionContentPartUnionParam {
	envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
	return []openai.ChatCompletionContentPartUnionParam{
		openai.TextContentPart(envelope),
	}
}

// convertViaFetchAsB64 fetches the URL and returns it as a base64 data-URL image part.
// On fetch error the attachment is soft-dropped (logged + nil returned).
func (c *Client) convertViaFetchAsB64(ctx context.Context, doc chat.Document) ([]openai.ChatCompletionContentPartUnionParam, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for b64", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineData = data
	return c.convertViaB64(fetched), nil
}

// convertViaFetchAsTXT fetches the URL and returns its content as a text envelope.
// On fetch error the attachment is soft-dropped (logged + nil returned).
func (c *Client) convertViaFetchAsTXT(ctx context.Context, doc chat.Document) ([]openai.ChatCompletionContentPartUnionParam, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for text", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineText = string(data)
	return c.convertViaTXT(fetched), nil
}
