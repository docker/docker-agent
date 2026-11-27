package anthropic

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func convertDocument(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) ([]ContentBlockParam, error) {
	mc := modelinfo.ResolveCaps(ctx, store, id, override)
	if override == nil && !mc.Supports(doc.MimeType) && modelinfo.IsClaude(ctx, store, id) {
		mc = modelinfo.CapsWith(true, true, false, false)
	}
	return convertDocumentWithCaps(ctx, doc, mc)
}

func convertDocumentWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]ContentBlockParam, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		mime := strings.ToLower(doc.MimeType)
		b64Data := base64.StdEncoding.EncodeToString(doc.Source.InlineData)

		if chat.IsImageMimeType(mime) {
			return []ContentBlockParam{
				{
					Type: "image",
					Source: &MediaSourceParam{
						Type:      "base64",
						Data:      b64Data,
						MediaType: mime,
					},
				},
			}, nil
		}

		// Gate PDF block strictly on application/pdf — IsAnthropicDocumentMime also
		// matches text/plain, which must NOT be sent as a document block.
		if mime == "application/pdf" {
			return []ContentBlockParam{
				{
					Type: "document",
					Source: &MediaSourceParam{
						Type:      "base64",
						Data:      b64Data,
						MediaType: "application/pdf",
					},
				},
			}, nil
		}

		slog.WarnContext(ctx, "anthropic: unexpected binary MIME in StrategyB64, dropping",
			"mime", doc.MimeType, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []ContentBlockParam{
			newTextBlock(envelope),
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}
