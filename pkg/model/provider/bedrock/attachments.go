package bedrock

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// imageFormatFromMIME maps a MIME type to a Bedrock ImageFormat.
// Returns ("", false) when the MIME type is not a supported image.
func imageFormatFromMIME(mimeType string) (types.ImageFormat, bool) {
	switch strings.ToLower(mimeType) {
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

// convertDocument converts a chat.Document to zero or more Bedrock ContentBlocks
// using the provided modelsdev.Store for capability lookup.
//
// Routing:
//   - image/* with InlineData → ContentBlockMemberImage
//   - application/pdf with InlineData → ContentBlockMemberDocument (PDF)
//   - text/* with InlineText → ContentBlockMemberText with TXTEnvelope
//   - unsupported / no content → nil (logged as warning)
func convertDocument(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store) ([]types.ContentBlock, error) {
	mc := modelinfo.LoadCaps(store, id)
	return convertDocumentWithCaps(ctx, doc, mc)
}

// convertDocumentWithCaps is the caps-injectable variant used by tests.
func convertDocumentWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]types.ContentBlock, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		mime := strings.ToLower(doc.MimeType)

		// Native image block
		if format, ok := imageFormatFromMIME(mime); ok {
			return []types.ContentBlock{
				&types.ContentBlockMemberImage{
					Value: types.ImageBlock{
						Format: format,
						Source: &types.ImageSourceMemberBytes{
							Value: doc.Source.InlineData,
						},
					},
				},
			}, nil
		}

		// Native PDF/document block
		if mime == "application/pdf" {
			return []types.ContentBlock{
				&types.ContentBlockMemberDocument{
					Value: types.DocumentBlock{
						Format: types.DocumentFormatPdf,
						Name:   aws.String(sanitizeDocumentName(doc.Name)),
						Source: &types.DocumentSourceMemberBytes{
							Value: doc.Source.InlineData,
						},
					},
				},
			}, nil
		}

		// Unexpected binary MIME — modelinfo should have filtered this out via
		// StrategyDrop, but guard defensively.
		slog.WarnContext(ctx, "bedrock: unexpected binary MIME in StrategyB64, dropping",
			"mime", doc.MimeType, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []types.ContentBlock{
			&types.ContentBlockMemberText{Value: envelope},
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}

// sanitizeDocumentName produces a Bedrock-safe document name.
// Bedrock allows alphanumeric characters, whitespace, hyphens, parentheses,
// and square brackets in DocumentBlock.name — but NOT dots.
// We strip the file extension first so "report.pdf" becomes "report" rather
// than the ugly "report-pdf", then replace any remaining disallowed characters
// with hyphens.
func sanitizeDocumentName(name string) string {
	// Strip the extension (e.g. ".pdf", ".docx") before sanitising so that
	// "report.pdf" → "report" instead of "report-pdf".
	base := strings.TrimSuffix(name, path.Ext(name))
	if base == "" {
		base = name // edge-case: name was just an extension (e.g. ".pdf")
	}

	var sb strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == ' ' || r == '(' || r == ')' || r == '[' || r == ']' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('-')
		}
	}
	result := strings.Trim(sb.String(), "-")
	if result == "" {
		return "document"
	}
	return result
}
