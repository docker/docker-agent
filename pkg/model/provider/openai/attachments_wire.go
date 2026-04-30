package openai

// This file wires attachment.Document support into the OpenAI Chat Completions
// and Responses API message converters.
//
// Wiring strategy for both APIs:
//   - expandDocumentPartsInMessages pre-processes each message's MultiContent,
//     expanding MessagePartTypeDocument parts into synthetic text/image parts
//     that the existing converters (oaistream.ConvertMessages and
//     convertMessagesToResponseInput) already understand.
//   - convertMessagesCtx and convertMessagesToResponseInputCtx use this
//     approach as a pre-pass, then delegate to the originals.
//
// This avoids duplicating the complex conversion logic in both paths.

import (
	"context"
	"log/slog"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
)

// expandDocumentParts expands any MessagePartTypeDocument entries in parts
// into synthetic text or image MessageParts that existing converters handle.
// On StrategyDrop the document is silently omitted (logged by convertDocument).
func (c *Client) expandDocumentParts(
	ctx context.Context,
	parts []chat.MessagePart,
) ([]chat.MessagePart, error) {
	expanded := make([]chat.MessagePart, 0, len(parts))
	for _, part := range parts {
		if part.Type != chat.MessagePartTypeDocument {
			expanded = append(expanded, part)
			continue
		}
		if part.Document == nil {
			continue
		}
		// Convert the document into native OpenAI parts.
		nativeParts, err := c.convertDocument(ctx, *part.Document)
		if err != nil {
			return nil, err
		}
		// Re-wrap each native part as a synthetic MessagePart so the existing
		// oaistream.ConvertMessages / convertMessagesToResponseInput can handle
		// them without modification.
		for _, np := range nativeParts {
			switch {
			case np.OfText != nil:
				expanded = append(expanded, chat.MessagePart{
					Type: chat.MessagePartTypeText,
					Text: np.OfText.Text,
				})
			case np.OfImageURL != nil && np.OfImageURL.ImageURL.URL != "":
				expanded = append(expanded, chat.MessagePart{
					Type:     chat.MessagePartTypeImageURL,
					ImageURL: &chat.MessageImageURL{URL: np.OfImageURL.ImageURL.URL},
				})
			default:
				slog.Warn("attachment: unhandled native part type, skipping", "name", part.Document.Name)
			}
		}
	}
	return expanded, nil
}

// expandDocumentPartsInMessages returns a copy of messages with all
// MessagePartTypeDocument parts expanded into synthetic text/image parts.
func (c *Client) expandDocumentPartsInMessages(
	ctx context.Context,
	messages []chat.Message,
) ([]chat.Message, error) {
	if !hasDocumentParts(messages) {
		return messages, nil
	}
	expanded := make([]chat.Message, len(messages))
	copy(expanded, messages)
	for i := range expanded {
		if len(expanded[i].MultiContent) == 0 {
			continue
		}
		newParts, err := c.expandDocumentParts(ctx, expanded[i].MultiContent)
		if err != nil {
			return nil, err
		}
		expanded[i].MultiContent = newParts
	}
	return expanded, nil
}

// convertMessagesCtx is like oaistream.ConvertMessages but also handles
// MessagePartTypeDocument parts by expanding them before conversion.
func (c *Client) convertMessagesCtx(
	ctx context.Context,
	messages []chat.Message,
) ([]openai.ChatCompletionMessageParamUnion, error) {
	expanded, err := c.expandDocumentPartsInMessages(ctx, messages)
	if err != nil {
		return nil, err
	}
	return oaistream.ConvertMessages(expanded), nil
}

// convertMessagesToResponseInputCtx is like convertMessagesToResponseInput but
// also handles MessagePartTypeDocument parts by expanding them before conversion.
// All the complex handling (orphan calls, tool image injection, etc.) is
// preserved by delegating to the original after document expansion.
func (c *Client) convertMessagesToResponseInputCtx(
	ctx context.Context,
	messages []chat.Message,
) ([]responses.ResponseInputItemUnionParam, error) {
	expanded, err := c.expandDocumentPartsInMessages(ctx, messages)
	if err != nil {
		return nil, err
	}
	return convertMessagesToResponseInput(expanded), nil
}

// hasDocumentParts reports whether any message in the slice contains a
// MessagePartTypeDocument part.
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
