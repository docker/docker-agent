package gemini

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// convertDocument converts a chat.Document to a Gemini genai.Part
// using the provided modelsdev.Store for capability lookup.
//
// Routing:
//   - image/* or binary with InlineData → genai.Blob part
//   - text MIMEs with InlineText → genai.Text part with TXTEnvelope
//   - unsupported / no content → nil (logged as warning)
func convertDocument(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) (*Part, error) {
	mc := modelinfo.ResolveCaps(ctx, store, id, override)
	return convertDocumentWithCaps(ctx, doc, mc)
}

// convertDocumentWithCaps is the caps-injectable variant used by tests.
func convertDocumentWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) (*Part, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		return NewPartFromBytes(doc.Source.InlineData, doc.MimeType), nil

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return NewPartFromText(envelope), nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}
