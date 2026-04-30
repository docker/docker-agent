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

// capabilityTableByProvider maps provider names (from ModelConfig.Provider) to
// their MIME-type capability tables. The openai and azure tables are identical;
// per-provider aliases (mistral, xai, ollama, nebius, minimax, github-copilot)
// each have their own entry.
//
// Phase 1: TXT / B64 / URL only — no Files API.
var capabilityTableByProvider = map[string]attachment.CapabilityTable{
	// OpenAI Chat Completions / Responses API
	// Note: application/pdf is intentionally absent in Phase 1.
	// OpenAI requires the Files API (not inline base64) for PDFs, which is Phase 2.
	"openai": {
		"image/jpeg":    {B64: true, URL: true},
		"image/png":     {B64: true, URL: true},
		"image/gif":     {B64: true, URL: true},
		"image/webp":    {B64: true, URL: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// Azure OpenAI — same capabilities as OpenAI (Phase 1).
	"azure": {
		"image/jpeg":    {B64: true, URL: true},
		"image/png":     {B64: true, URL: true},
		"image/gif":     {B64: true, URL: true},
		"image/webp":    {B64: true, URL: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// Mistral — PDF via URL only (document_url); images via B64 or URL; no gif
	"mistral": {
		"image/jpeg":      {B64: true, URL: true},
		"image/png":       {B64: true, URL: true},
		"image/webp":      {B64: true, URL: true},
		"application/pdf": {URL: true},
		"text/plain":      {TXT: true},
		"text/markdown":   {TXT: true},
		"text/html":       {TXT: true},
		"text/csv":        {TXT: true},
	},
	// xAI (Grok) — images B64/URL; text only; no PDF
	"xai": {
		"image/jpeg":    {B64: true, URL: true},
		"image/png":     {B64: true, URL: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// Ollama — images B64 only; text only; no PDF, no URL images
	"ollama": {
		"image/jpeg":    {B64: true},
		"image/png":     {B64: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// Nebius — same as ollama
	"nebius": {
		"image/jpeg":    {B64: true},
		"image/png":     {B64: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// MiniMax — same as ollama
	"minimax": {
		"image/jpeg":    {B64: true},
		"image/png":     {B64: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// GitHub Copilot — same as ollama
	"github-copilot": {
		"image/jpeg":    {B64: true},
		"image/png":     {B64: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
	// Requesty — same as openai (acts as a proxy). No PDF in Phase 1.
	"requesty": {
		"image/jpeg":    {B64: true, URL: true},
		"image/png":     {B64: true, URL: true},
		"image/gif":     {B64: true, URL: true},
		"image/webp":    {B64: true, URL: true},
		"text/plain":    {TXT: true},
		"text/markdown": {TXT: true},
		"text/html":     {TXT: true},
		"text/csv":      {TXT: true},
	},
}

// capabilityTable returns the capability table for this client's provider,
// falling back to the openai table for unknown providers.
func (c *Client) capabilityTable() attachment.CapabilityTable {
	if table, ok := capabilityTableByProvider[c.ModelConfig.Provider]; ok {
		return table
	}
	return capabilityTableByProvider["openai"]
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
func (c *Client) convertViaURL(doc chat.Document) []openai.ChatCompletionContentPartUnionParam {
	return []openai.ChatCompletionContentPartUnionParam{
		openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL: doc.Source.URL,
		}),
	}
}

// convertViaB64 encodes inline binary as a base64 data-URL image part.
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
