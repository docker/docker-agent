package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// convertDocumentToResponseInput converts a chat.Document to zero or
// more ResponseInputContentUnionParam values for the OpenAI Responses API,
// using the provided modelsdev.Store for capability lookup.
//
// Routing:
//   - image/* with InlineData → OfInputImage with a data URI
//   - application/pdf with InlineData → OfInputFile (base64)
//   - text MIMEs with InlineText → OfInputText with TXTEnvelope
//   - unsupported / no content → nil (logged as warning)
func convertDocumentToResponseInput(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store) ([]responses.ResponseInputContentUnionParam, error) {
	mc := modelinfo.LoadCaps(store, id)
	return convertDocumentToResponseInputWithCaps(ctx, doc, mc)
}

// convertDocumentToResponseInputWithCaps is the caps-injectable variant used by tests.
func convertDocumentToResponseInputWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]responses.ResponseInputContentUnionParam, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		mime := strings.ToLower(doc.MimeType)
		if strings.HasPrefix(mime, "image/") {
			dataURI := fmt.Sprintf("data:%s;base64,%s",
				doc.MimeType,
				base64.StdEncoding.EncodeToString(doc.Source.InlineData))
			return []responses.ResponseInputContentUnionParam{
				{
					OfInputImage: &responses.ResponseInputImageParam{
						ImageURL: param.NewOpt(dataURI),
						Detail:   responses.ResponseInputImageDetail(responses.ResponseInputImageContentDetailAuto),
					},
				},
			}, nil
		}

		if mime == "application/pdf" {
			// The Responses API file block expects raw base64-encoded bytes in FileData
			// (not a data URI). See ResponseInputFileParam.FileData godoc:
			// "The base64-encoded data of the file to be sent to the model."
			b64Data := base64.StdEncoding.EncodeToString(doc.Source.InlineData)
			return []responses.ResponseInputContentUnionParam{
				{
					OfInputFile: &responses.ResponseInputFileParam{
						FileData: param.NewOpt(b64Data),
						Filename: param.NewOpt(doc.Name),
					},
				},
			}, nil
		}

		// Unexpected binary MIME — defensive drop.
		slog.WarnContext(ctx, "openai responses: unexpected binary MIME in StrategyB64, dropping",
			"mime", doc.MimeType, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []responses.ResponseInputContentUnionParam{
			{OfInputText: &responses.ResponseInputTextParam{Text: envelope}},
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}
