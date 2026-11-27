package oaistream

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"mime"
	"strings"

	"github.com/docker/docker-agent/pkg/attachment"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

func convertDocumentWithCaps(ctx context.Context, doc chat.Document, mc modelinfo.ModelCapabilities) ([]ContentPart, error) {
	strategy, reason := attachment.Decide(doc, mc)

	switch strategy {
	case attachment.StrategyDrop:
		slog.WarnContext(ctx, "attachment dropped", "reason", reason, "doc", doc.Name)
		return nil, nil

	case attachment.StrategyB64:
		switch mediaType := strings.ToLower(doc.MimeType); {
		case strings.HasPrefix(mediaType, "image/"):
			return []ContentPart{
				ImageContentPart(ImageURLParam{
					URL: dataURI(doc.MimeType, doc.Source.InlineData),
				}),
			}, nil
		case mediaType == "application/pdf":
			return []ContentPart{
				FileContentPart(FileParam{
					FileData: dataURI(doc.MimeType, doc.Source.InlineData),
					Filename: doc.Name,
				}),
			}, nil
		case strings.HasPrefix(mediaType, "audio/"):
			if format := inputAudioFormat(doc.MimeType); format != "" {
				return []ContentPart{
					InputAudioContentPart(InputAudioParam{
						Data: base64.StdEncoding.EncodeToString(doc.Source.InlineData), Format: format,
					}),
				}, nil
			}
			slog.WarnContext(ctx, "oaistream: unsupported audio format for Chat Completions endpoint — dropping", "mime", doc.MimeType, "doc", doc.Name)
			return nil, nil
		default:
			slog.WarnContext(ctx, "oaistream: unsupported binary attachment for Chat Completions endpoint — dropping",
				"mime", doc.MimeType, "doc", doc.Name)
			return nil, nil
		}

	case attachment.StrategyTXT:
		envelope := attachment.TXTEnvelope(doc.Name, doc.MimeType, doc.Source.InlineText)
		return []ContentPart{
			TextContentPart(envelope),
		}, nil

	default:
		return nil, fmt.Errorf("unknown attachment strategy %d", strategy)
	}
}

func dataURI(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func inputAudioFormat(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	switch mediaType {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav"
	case "audio/mp3", "audio/mpeg":
		return "mp3"
	default:
		return ""
	}
}
