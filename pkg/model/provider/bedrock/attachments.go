package bedrock

// attachments.go — Phase 1 attachment support for the Bedrock provider.
//
// Wire-up: convertUserContentCtx replaces convertUserContent in the context-aware
// convertMessagesCtx method, handling MessagePartTypeDocument parts by calling
// convertDocument and injecting the resulting ContentBlocks inline.
// The existing package-level convertMessages / convertUserContent functions are
// unchanged so pre-existing tests continue to compile and pass.

import (
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
)

// capabilityTable is the Bedrock Phase 1 capability table.
// Bedrock Converse API: images and PDFs as B64 bytes; text documents as TXT envelopes.
// Phase 1: no URL delivery, no Files API.
var capabilityTable = attachment.CapabilityTable{
	"image/jpeg":      {B64: true},
	"image/png":       {B64: true},
	"image/gif":       {B64: true},
	"image/webp":      {B64: true},
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

// convertDocument converts a chat.Document into zero or more types.ContentBlock values.
//
// On StrategyDrop the attachment is silently skipped (a Warn is logged).
func (c *Client) convertDocument(ctx context.Context, doc chat.Document) ([]types.ContentBlock, error) {
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

	default: // StrategyDrop (and StrategyURL — not in Bedrock table)
		slog.Warn("attachment: dropping document", "name", doc.Name, "mime", doc.MimeType, "reason", reason)
		return nil, nil
	}
}

// mimeToImageFormat converts a MIME type to a Bedrock ImageFormat.
func mimeToImageFormat(mimeType string) (types.ImageFormat, bool) {
	switch mimeType {
	case "image/jpeg":
		return types.ImageFormatJpeg, true
	case "image/png":
		return types.ImageFormatPng, true
	case "image/gif":
		return types.ImageFormatGif, true
	case "image/webp":
		return types.ImageFormatWebp, true
	default:
		return "", false
	}
}

// mimeToDocumentFormat converts a MIME type to a Bedrock DocumentFormat.
func mimeToDocumentFormat(mimeType string) (types.DocumentFormat, bool) {
	switch mimeType {
	case "application/pdf":
		return types.DocumentFormatPdf, true
	case "text/plain":
		return types.DocumentFormatTxt, true
	case "text/markdown":
		return types.DocumentFormatMd, true
	case "text/html":
		return types.DocumentFormatHtml, true
	case "text/csv":
		return types.DocumentFormatCsv, true
	default:
		return "", false
	}
}

// sanitizeDocName removes characters disallowed by Bedrock's DocumentBlock.Name
// constraint: only alphanumerics, single spaces, hyphens, parentheses, and
// square brackets are permitted.
var invalidDocNameChar = regexp.MustCompile(`[^a-zA-Z0-9 \-()\[\]]`)

func sanitizeDocName(name string) string {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	if base == "" || base == "." {
		return "document"
	}
	clean := invalidDocNameChar.ReplaceAllString(base, "-")
	// Collapse runs of spaces to a single space (Bedrock requirement).
	result := strings.Join(strings.Fields(clean), " ")
	if result == "" {
		return "document"
	}
	return result
}

// convertDocViaB64 encodes inline binary and returns the appropriate Bedrock block.
// Images → ImageBlock; documents (PDF etc.) → DocumentBlock with bytes source.
func convertDocViaB64(doc chat.Document) []types.ContentBlock {
	if format, ok := mimeToImageFormat(doc.MimeType); ok {
		return []types.ContentBlock{
			&types.ContentBlockMemberImage{
				Value: types.ImageBlock{
					Format: format,
					Source: &types.ImageSourceMemberBytes{
						Value: doc.Source.InlineData,
					},
				},
			},
		}
	}

	if format, ok := mimeToDocumentFormat(doc.MimeType); ok {
		return []types.ContentBlock{
			&types.ContentBlockMemberDocument{
				Value: types.DocumentBlock{
					Name:   aws.String(sanitizeDocName(doc.Name)),
					Format: format,
					Source: &types.DocumentSourceMemberBytes{
						Value: doc.Source.InlineData,
					},
				},
			},
		}
	}

	slog.Warn("attachment: bedrock cannot encode mime as B64", "mime", doc.MimeType)
	return nil
}

// convertDocViaTXT wraps inline text in an XML envelope and returns a text block.
func convertDocViaTXT(doc chat.Document) []types.ContentBlock {
	envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
	return []types.ContentBlock{
		&types.ContentBlockMemberText{Value: envelope},
	}
}

// convertDocViaFetchAsB64 fetches the URL and returns the bytes as a B64 block.
func convertDocViaFetchAsB64(ctx context.Context, doc chat.Document) ([]types.ContentBlock, error) {
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
func convertDocViaFetchAsTXT(ctx context.Context, doc chat.Document) ([]types.ContentBlock, error) {
	data, err := attachment.FetchURL(ctx, doc.Source.URL)
	if err != nil {
		slog.Warn("attachment: failed to fetch url for text", "name", doc.Name, "url", doc.Source.URL, "error", err)
		return nil, nil
	}
	fetched := doc
	fetched.Source.InlineText = string(data)
	return convertDocViaTXT(fetched), nil
}

// convertUserContentCtx is the context-aware variant of convertUserContent.
// It handles MessagePartTypeDocument parts via convertDocument, passing all
// other parts to the existing convertImageURL / text logic.
func (c *Client) convertUserContentCtx(ctx context.Context, msg *chat.Message) ([]types.ContentBlock, error) {
	if len(msg.MultiContent) == 0 {
		return []types.ContentBlock{
			&types.ContentBlockMemberText{Value: msg.Content},
		}, nil
	}

	var blocks []types.ContentBlock
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			blocks = append(blocks, &types.ContentBlockMemberText{Value: part.Text})

		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil {
				if imageBlock := convertImageURL(part.ImageURL); imageBlock != nil {
					blocks = append(blocks, imageBlock)
				}
			}

		case chat.MessagePartTypeDocument:
			if part.Document == nil {
				continue
			}
			docBlocks, err := c.convertDocument(ctx, *part.Document)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, docBlocks...)
		}
	}
	return blocks, nil
}

// hasDocumentParts reports whether any message contains a document part.
func hasDocumentParts(messages []chat.Message) bool {
	for i := range messages {
		for _, part := range messages[i].MultiContent {
			if part.Type == chat.MessagePartTypeDocument {
				return true
			}
		}
	}
	return false
}

// convertMessagesCtx is the context-aware variant of the package-level
// convertMessages function. It handles document parts; everything else
// delegates to the existing logic.
func (c *Client) convertMessagesCtx(ctx context.Context, messages []chat.Message, enableCaching bool) ([]types.Message, []types.SystemContentBlock, error) {
	if !hasDocumentParts(messages) {
		msgs, sys := convertMessages(messages, enableCaching)
		return msgs, sys, nil
	}

	var bedrockMessages []types.Message
	var systemBlocks []types.SystemContentBlock

	for i := 0; i < len(messages); i++ {
		msg := &messages[i]

		switch msg.Role {
		case chat.MessageRoleSystem:
			if len(msg.MultiContent) > 0 {
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{Value: part.Text})
					}
				}
			} else {
				systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{Value: msg.Content})
			}

		case chat.MessageRoleUser:
			contentBlocks, err := c.convertUserContentCtx(ctx, msg)
			if err != nil {
				return nil, nil, err
			}
			if len(contentBlocks) > 0 {
				bedrockMessages = append(bedrockMessages, types.Message{
					Role:    types.ConversationRoleUser,
					Content: contentBlocks,
				})
			}

		case chat.MessageRoleAssistant:
			contentBlocks := convertAssistantContent(msg)
			if len(contentBlocks) > 0 {
				bedrockMessages = append(bedrockMessages, types.Message{
					Role:    types.ConversationRoleAssistant,
					Content: contentBlocks,
				})
			}

		case chat.MessageRoleTool:
			var toolResultBlocks []types.ContentBlock
			j := i
			for j < len(messages) && messages[j].Role == chat.MessageRoleTool {
				if messages[j].ToolCallID != "" {
					toolResultBlocks = append(toolResultBlocks, &types.ContentBlockMemberToolResult{
						Value: types.ToolResultBlock{
							ToolUseId: aws.String(messages[j].ToolCallID),
							Content: []types.ToolResultContentBlock{
								&types.ToolResultContentBlockMemberText{Value: messages[j].Content},
							},
						},
					})
				}
				j++
			}
			if len(toolResultBlocks) > 0 {
				bedrockMessages = append(bedrockMessages, types.Message{
					Role:    types.ConversationRoleUser,
					Content: toolResultBlocks,
				})
			}
			i = j - 1
		}
	}

	if enableCaching {
		if len(systemBlocks) > 0 {
			systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
		}
		applyCachePointsToMessages(bedrockMessages)
	}

	return bedrockMessages, systemBlocks, nil
}
