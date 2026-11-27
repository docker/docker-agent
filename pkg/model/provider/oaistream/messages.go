package oaistream

/*
This file contains shared message conversion utilities for OpenAI-compatible providers.
*/

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// JSONSchema is a helper type that implements json.Marshaler for map[string]any.
type JSONSchema map[string]any

func (j JSONSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}

func ConvertMultiContent(ctx context.Context, multiContent []chat.MessagePart, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) []ContentPart {
	return convertMultiContentWithCaps(ctx, multiContent, modelinfo.ResolveCaps(ctx, store, id, override))
}

func ConvertMessages(ctx context.Context, messages []chat.Message, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) []ChatMessage {
	return ConvertMessagesWithTransform(ctx, messages, modelinfo.ResolveCaps(ctx, store, id, override), nil)
}

func ConvertMessagesWithCaps(ctx context.Context, messages []chat.Message, mc modelinfo.ModelCapabilities) []ChatMessage {
	return ConvertMessagesWithTransform(ctx, messages, mc, nil)
}

func convertMultiContentWithCaps(ctx context.Context, multiContent []chat.MessagePart, mc modelinfo.ModelCapabilities) []ContentPart {
	parts := make([]ContentPart, 0, len(multiContent))
	for _, part := range multiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			parts = append(parts, TextContentPart(part.Text))
		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil {
				parts = append(parts, ImageContentPart(ImageURLParam{
					URL:    part.ImageURL.URL,
					Detail: string(part.ImageURL.Detail),
				}))
			}
		case chat.MessagePartTypeDocument:
			if part.Document != nil {
				docParts, err := convertDocumentWithCaps(ctx, *part.Document, mc)
				if err != nil {
					slog.WarnContext(ctx, "failed to convert document attachment", "error", err, "doc", part.Document.Name)
					continue
				}
				parts = append(parts, docParts...)
			}
		}
	}
	return parts
}

func ConvertMessagesWithTransform(ctx context.Context, messages []chat.Message, mc modelinfo.ModelCapabilities, transform func(int, []ChatMessage)) []ChatMessage {
	openaiMessages := make([]ChatMessage, 0, len(messages))
	var deferred []ChatMessage
	for i := range messages {
		msg := &messages[i]

		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) == 0 && len(msg.MultiContent) == 0 && strings.TrimSpace(msg.Content) == "" && strings.TrimSpace(msg.ReasoningContent) == "" {
			continue
		}
		if msg.Role != chat.MessageRoleTool {
			openaiMessages = append(openaiMessages, deferred...)
			deferred = nil
		}

		var openaiMessage ChatMessage
		var attachmentParts []ContentPart

		switch msg.Role {
		case chat.MessageRoleSystem:
			if len(msg.MultiContent) == 0 {
				openaiMessage = SystemMessage(msg.Content)
			} else {
				textParts := make([]ContentPart, 0)
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						textParts = append(textParts, TextContentPart(part.Text))
					}
				}
				openaiMessage = SystemMultiMessage(textParts)
			}

		case chat.MessageRoleUser:
			if len(msg.MultiContent) == 0 {
				openaiMessage = UserMessage(msg.Content)
			} else {
				openaiMessage = UserMultiMessage(convertMultiContentWithCaps(ctx, msg.MultiContent, mc))
			}

		case chat.MessageRoleAssistant:
			assistant := ChatMessage{Role: "assistant"}
			hasContent := false

			if len(msg.MultiContent) == 0 {
				if msg.Content != "" {
					assistant.Content = new(msg.Content)
					hasContent = true
				}
			} else {
				contentParts := make([]ContentPart, 0)
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						contentParts = append(contentParts, TextContentPart(part.Text))
					}
				}
				if len(contentParts) > 0 {
					assistant.ContentParts = contentParts
					hasContent = true
				}
			}

			if strings.TrimSpace(msg.ReasoningContent) != "" {
				assistant.ReasoningContent = msg.ReasoningContent
			}

			if msg.FunctionCall != nil {
				assistant.FunctionCall = &FunctionCallParam{
					Name:      msg.FunctionCall.Name,
					Arguments: msg.FunctionCall.Arguments,
				}
			}

			if len(msg.ToolCalls) > 0 {
				toolCalls := make([]ToolCallParam, len(msg.ToolCalls))
				for j, toolCall := range msg.ToolCalls {
					toolCalls[j] = ToolCallParam{
						ID:   toolCall.ID,
						Type: "function",
						Function: FunctionCallParam{
							Name:      toolCall.Function.Name,
							Arguments: toolCall.Function.Arguments,
						},
					}
				}
				assistant.ToolCalls = toolCalls
			}

			if !hasContent && len(assistant.ToolCalls) == 0 {
				assistant.Content = new("")
			}

			openaiMessage = assistant

		case chat.MessageRoleTool:
			toolMsg := ChatMessage{
				Role:       "tool",
				ToolCallID: msg.ToolCallID,
			}

			if len(msg.MultiContent) == 0 {
				toolMsg.Content = new(msg.Content)
			} else {
				textParts := make([]ContentPart, 0)
				for _, part := range msg.MultiContent {
					switch part.Type {
					case chat.MessagePartTypeText:
						textParts = append(textParts, TextContentPart(part.Text))
					case chat.MessagePartTypeImageURL:
						if part.ImageURL != nil {
							attachmentParts = append(attachmentParts, ImageContentPart(ImageURLParam{
								URL:    part.ImageURL.URL,
								Detail: string(part.ImageURL.Detail),
							}))
						}
					case chat.MessagePartTypeDocument:
						if part.Document != nil {
							docParts, err := convertDocumentWithCaps(ctx, *part.Document, mc)
							if err != nil {
								slog.WarnContext(ctx, "failed to convert tool result document attachment", "error", err, "doc", part.Document.Name)
								continue
							}
							attachmentParts = append(attachmentParts, docParts...)
						}
					}
				}
				if len(textParts) == 0 {
					toolMsg.Content = new(msg.Content)
				} else {
					toolMsg.ContentParts = textParts
				}
			}

			openaiMessage = toolMsg
		}

		converted := []ChatMessage{openaiMessage}
		if len(attachmentParts) > 0 {
			label := TextContentPart("Attached content from tool result (tool_call_id=" + msg.ToolCallID + "):")
			allParts := append([]ContentPart{label}, attachmentParts...)
			converted = append(converted, UserMultiMessage(allParts))
		}
		if transform != nil {
			transform(i, converted)
		}
		openaiMessages = append(openaiMessages, converted[0])
		deferred = append(deferred, converted[1:]...)
	}
	openaiMessages = append(openaiMessages, deferred...)
	return openaiMessages
}

// MergeConsecutiveMessages merges consecutive system or user messages into single messages.
func MergeConsecutiveMessages(openaiMessages []ChatMessage) []ChatMessage {
	mergedMessages := make([]ChatMessage, 0, len(openaiMessages))

	for i := 0; i < len(openaiMessages); {
		currentRole := openaiMessages[i].Role
		if currentRole != "system" && currentRole != "user" {
			mergedMessages = append(mergedMessages, openaiMessages[i])
			i++
			continue
		}

		j := i + 1
		for j < len(openaiMessages) && openaiMessages[j].Role == currentRole {
			j++
		}

		if j == i+1 || hasMergeBarrier(openaiMessages[i:j]) {
			mergedMessages = append(mergedMessages, openaiMessages[i:j]...)
			i = j
			continue
		}

		mergedMessages = append(mergedMessages, mergeMessageRun(openaiMessages[i:j], currentRole))
		i = j
	}

	return mergedMessages
}

func hasMergeBarrier(messages []ChatMessage) bool {
	for _, msg := range messages {
		if msg.Name != "" || len(msg.ExtraFields) > 0 {
			return true
		}
		if slices.ContainsFunc(msg.ContentParts, func(p ContentPart) bool {
			return len(p.ExtraFields) > 0
		}) {
			return true
		}
	}
	return false
}

func mergeMessageRun(messages []ChatMessage, role string) ChatMessage {
	var mergedContent strings.Builder
	var mergedMultiContent []ContentPart

	for _, msg := range messages {
		if len(msg.ContentParts) > 0 {
			if mergedContent.Len() > 0 {
				mergedMultiContent = append(mergedMultiContent, TextContentPart(mergedContent.String()))
				mergedContent.Reset()
			}
			mergedMultiContent = append(mergedMultiContent, msg.ContentParts...)
		} else if msg.Content != nil && *msg.Content != "" {
			if mergedContent.Len() > 0 {
				mergedContent.WriteString("\n")
			}
			mergedContent.WriteString(*msg.Content)
		}
	}
	if len(mergedMultiContent) > 0 && mergedContent.Len() > 0 {
		mergedMultiContent = append(mergedMultiContent, TextContentPart(mergedContent.String()))
	}

	if role == "system" {
		if len(mergedMultiContent) == 0 {
			return SystemMessage(mergedContent.String())
		}
		textParts := make([]ContentPart, 0, len(mergedMultiContent))
		for _, part := range mergedMultiContent {
			if part.Type == "text" {
				textParts = append(textParts, part)
			}
		}
		return SystemMultiMessage(textParts)
	}
	if len(mergedMultiContent) == 0 {
		return UserMessage(mergedContent.String())
	}
	return UserMultiMessage(mergedMultiContent)
}
