package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"slices"
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
//   - application/pdf with InlineData → OfInputFile with a data URI
//   - text MIMEs with InlineText → OfInputText with TXTEnvelope
//   - unsupported / no content → nil (logged as warning)
func convertDocumentToResponseInput(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) ([]responses.ResponseInputContentUnionParam, error) {
	mc := modelinfo.ResolveCaps(ctx, store, id, override)
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
			dataURI := "data:" + doc.MimeType + ";base64," + base64.StdEncoding.EncodeToString(doc.Source.InlineData)
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
			dataURI := "data:" + doc.MimeType + ";base64," + base64.StdEncoding.EncodeToString(doc.Source.InlineData)
			return []responses.ResponseInputContentUnionParam{
				{
					OfInputFile: &responses.ResponseInputFileParam{
						FileData: param.NewOpt(dataURI),
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

// convertToolResultOutput keeps multimodal content associated with its call ID.
func (c *Client) convertToolResultOutput(ctx context.Context, msg chat.Message) responses.ResponseInputItemFunctionCallOutputOutputUnionParam {
	var content responses.ResponseFunctionCallOutputItemListParam
	if msg.Content != "" && !slices.ContainsFunc(msg.MultiContent, func(p chat.MessagePart) bool { return p.Type == chat.MessagePartTypeText }) {
		content = append(content, responses.ResponseFunctionCallOutputItemUnionParam{OfInputText: &responses.ResponseInputTextContentParam{Text: msg.Content}})
	}
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			content = append(content, responses.ResponseFunctionCallOutputItemUnionParam{
				OfInputText: &responses.ResponseInputTextContentParam{Text: part.Text},
			})
		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil {
				detail := responses.ResponseInputImageContentDetailAuto
				switch part.ImageURL.Detail {
				case chat.ImageURLDetailHigh:
					detail = responses.ResponseInputImageContentDetailHigh
				case chat.ImageURLDetailLow:
					detail = responses.ResponseInputImageContentDetailLow
				}
				content = append(content, responses.ResponseFunctionCallOutputItemUnionParam{
					OfInputImage: &responses.ResponseInputImageContentParam{ImageURL: param.NewOpt(part.ImageURL.URL), Detail: detail},
				})
			}
		case chat.MessagePartTypeDocument:
			if part.Document == nil {
				continue
			}
			parts, err := convertDocumentToResponseInput(ctx, *part.Document, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
			if err != nil {
				slog.WarnContext(ctx, "failed to convert tool result document attachment", "error", err, "doc", part.Document.Name)
				continue
			}
			for _, p := range parts {
				switch {
				case p.OfInputText != nil:
					content = append(content, responses.ResponseFunctionCallOutputItemUnionParam{OfInputText: &responses.ResponseInputTextContentParam{Text: p.OfInputText.Text}})
				case p.OfInputImage != nil:
					content = append(content, responses.ResponseFunctionCallOutputItemUnionParam{OfInputImage: &responses.ResponseInputImageContentParam{
						ImageURL: p.OfInputImage.ImageURL, FileID: p.OfInputImage.FileID, Detail: responses.ResponseInputImageContentDetail(p.OfInputImage.Detail),
					}})
				case p.OfInputFile != nil:
					content = append(content, responses.ResponseFunctionCallOutputItemUnionParam{OfInputFile: &responses.ResponseInputFileContentParam{
						FileData: p.OfInputFile.FileData, FileID: p.OfInputFile.FileID, FileURL: p.OfInputFile.FileURL, Filename: p.OfInputFile.Filename,
					}})
				}
			}
		}
	}
	if len(content) == 0 {
		return responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(msg.Content)}
	}
	if len(content) == 1 && content[0].OfInputText != nil {
		return responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(content[0].OfInputText.Text)}
	}
	return responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfResponseFunctionCallOutputItemArray: content}
}
