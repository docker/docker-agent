package anthropic

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// convertDocument converts a chat.Document to standard Anthropic SDK content blocks
// using an explicit modelsdev.Store for capability lookup.
//
// Routing:
//   - image/* with InlineData → ImageBlockParam (base64 source)
//   - application/pdf with InlineData → DocumentBlockParam (base64)
//   - text with InlineText → TextBlockParam with TXTEnvelope
//   - unsupported / no content → nil (logged as warning)
func convertDocument(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store) ([]anthropic.ContentBlockParamUnion, error) {
	mc := modelinfo.LoadCaps(store, id)
	return convertDocumentWithCaps(ctx, doc, mc)
}

// convertDocumentWithCaps is the caps-injectable variant used by tests.
func convertDocumentWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]anthropic.ContentBlockParamUnion, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		mime := strings.ToLower(doc.MimeType)
		b64Data := base64.StdEncoding.EncodeToString(doc.Source.InlineData)

		if IsImageMime(mime) {
			return []anthropic.ContentBlockParamUnion{
				{
					OfImage: &anthropic.ImageBlockParam{
						Source: anthropic.ImageBlockParamSourceUnion{
							OfBase64: &anthropic.Base64ImageSourceParam{
								Data:      b64Data,
								MediaType: anthropic.Base64ImageSourceMediaType(mime),
							},
						},
					},
				},
			}, nil
		}

		// Gate PDF block strictly on application/pdf — IsAnthropicDocumentMime also
		// matches text/plain, which must NOT be sent as a DocumentBlockParam.
		if mime == "application/pdf" {
			return []anthropic.ContentBlockParamUnion{
				{
					OfDocument: &anthropic.DocumentBlockParam{
						Source: anthropic.DocumentBlockParamSourceUnion{
							OfBase64: &anthropic.Base64PDFSourceParam{
								Data:      b64Data,
								MediaType: "application/pdf",
							},
						},
					},
				},
			}, nil
		}

		// Unexpected binary MIME — defensive drop.
		slog.WarnContext(ctx, "anthropic: unexpected binary MIME in StrategyB64, dropping",
			"mime", doc.MimeType, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []anthropic.ContentBlockParamUnion{
			{OfText: &anthropic.TextBlockParam{Text: envelope}},
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}
