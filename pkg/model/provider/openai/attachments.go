package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func convertDocumentToResponseInput(ctx context.Context, doc chat.Document, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) ([]ResponseContentPart, error) {
	mc := modelinfo.ResolveCaps(ctx, store, id, override)
	return convertDocumentToResponseInputWithCaps(ctx, doc, mc)
}

func convertDocumentToResponseInputWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]ResponseContentPart, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		mime := strings.ToLower(doc.MimeType)
		if strings.HasPrefix(mime, "image/") {
			dataURI := "data:" + doc.MimeType + ";base64," + base64.StdEncoding.EncodeToString(doc.Source.InlineData)
			return []ResponseContentPart{
				{
					Type:     "input_image",
					ImageURL: dataURI,
					Detail:   "auto",
				},
			}, nil
		}

		if mime == "application/pdf" {
			dataURI := "data:" + doc.MimeType + ";base64," + base64.StdEncoding.EncodeToString(doc.Source.InlineData)
			return []ResponseContentPart{
				{
					Type:     "input_file",
					FileData: dataURI,
					Filename: doc.Name,
				},
			}, nil
		}

		slog.WarnContext(ctx, "openai responses: unexpected binary MIME in StrategyB64, dropping",
			"mime", doc.MimeType, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []ResponseContentPart{
			{Type: "input_text", Text: envelope},
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}

// convertToolResultOutput keeps multimodal content associated with its call ID.
func (c *Client) convertToolResultOutput(ctx context.Context, msg chat.Message) (*string, []ResponseContentPart) {
	var content []ResponseContentPart
	if msg.Content != "" && !slices.ContainsFunc(msg.MultiContent, func(p chat.MessagePart) bool { return p.Type == chat.MessagePartTypeText }) {
		content = append(content, ResponseContentPart{Type: "input_text", Text: msg.Content})
	}
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			content = append(content, ResponseContentPart{Type: "input_text", Text: part.Text})
		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil {
				detail := "auto"
				switch part.ImageURL.Detail {
				case chat.ImageURLDetailHigh:
					detail = "high"
				case chat.ImageURLDetailLow:
					detail = "low"
				}
				content = append(content, ResponseContentPart{
					Type:     "input_image",
					ImageURL: part.ImageURL.URL,
					Detail:   detail,
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
			content = append(content, parts...)
		}
	}
	if len(content) == 0 {
		return new(msg.Content), nil
	}
	if len(content) == 1 && content[0].Type == "input_text" {
		return new(content[0].Text), nil
	}
	return nil, content
}
