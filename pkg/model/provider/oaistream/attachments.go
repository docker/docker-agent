package oaistream

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// convertDocumentWithCaps converts a chat.Document to zero or more
// ChatCompletionContentPartUnionParam values using the OpenAI Chat Completions
// format, given the model's resolved attachment capabilities.
//
// Routing:
//   - image/* with InlineData → data-URI image part
//   - other binary MIMEs with InlineData → drop (no native document block on Chat Completions)
//   - text MIMEs with InlineText → text part with TXTEnvelope
//   - unsupported / no content → nil (logged as warning)
func convertDocumentWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]openai.ChatCompletionContentPartUnionParam, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		// The data URI is only built in the branches that use it, so an
		// unsupported binary MIME is dropped without paying for base64 encoding.
		switch mime := strings.ToLower(doc.MimeType); {
		case strings.HasPrefix(mime, "image/"):
			return []openai.ChatCompletionContentPartUnionParam{
				openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: dataURI(doc.MimeType, doc.Source.InlineData),
				}),
			}, nil
		case mime == "application/pdf":
			// Chat Completions accepts PDFs as a `file` content part carrying a
			// base64 data URI. Decide only routes a PDF here when the model
			// declares PDF support, so models that cannot read PDFs are dropped
			// upstream and never reach this branch.
			return []openai.ChatCompletionContentPartUnionParam{
				openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
					FileData: openai.String(dataURI(doc.MimeType, doc.Source.InlineData)),
					Filename: openai.String(doc.Name),
				}),
			}, nil
		default:
			// Any other binary MIME with inline data has no native Chat
			// Completions representation; drop and warn.
			slog.WarnContext(ctx, "oaistream: unsupported binary attachment for Chat Completions endpoint — dropping",
				"mime", doc.MimeType, "doc", doc.Name)
			return nil, nil
		}

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []openai.ChatCompletionContentPartUnionParam{
			openai.TextContentPart(envelope),
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}

// dataURI builds an RFC 2397 base64 data URI for the given MIME type and bytes.
func dataURI(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
