package oaistream

/*
This file contains shared message conversion utilities for OpenAI-compatible providers.
*/

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// JSONSchema is a helper type that implements json.Marshaler for map[string]any.
// This allows us to pass schema maps to the OpenAI library which expects json.Marshaler.
type JSONSchema map[string]any

// MarshalJSON implements json.Marshaler for JSONSchema.
func (j JSONSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}

// ConvertMultiContent converts chat.MessagePart slices to OpenAI content
// parts, resolving attachment capabilities from the provided modelsdev.Store.
// A non-nil override declares the model's attachment capabilities explicitly,
// bypassing the models.dev lookup (issue #2741).
func ConvertMultiContent(ctx context.Context, multiContent []chat.MessagePart, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) []openai.ChatCompletionContentPartUnionParam {
	return convertMultiContentWithCaps(ctx, multiContent, modelinfo.ResolveCaps(ctx, store, id, override))
}

// ConvertMessages converts chat.Message slices to OpenAI message params,
// resolving attachment capabilities from the provided modelsdev.Store. A
// non-nil override declares the model's attachment capabilities explicitly,
// bypassing the models.dev lookup (issue #2741).
func ConvertMessages(ctx context.Context, messages []chat.Message, id modelsdev.ID, store *modelsdev.Store, override *modelinfo.CapsOverride) []openai.ChatCompletionMessageParamUnion {
	return convertMessagesWithCaps(ctx, messages, modelinfo.ResolveCaps(ctx, store, id, override))
}

// ConvertMessagesWithCaps is the caps-injectable variant of [ConvertMessages].
// It is used by providers whose models are not in the models.dev catalog (e.g.
// Docker Model Runner), which must supply attachment capabilities explicitly
// rather than resolving them from a store.
func ConvertMessagesWithCaps(ctx context.Context, messages []chat.Message, mc modelinfo.ModelCapabilities) []openai.ChatCompletionMessageParamUnion {
	return convertMessagesWithCaps(ctx, messages, mc)
}

func convertMultiContentWithCaps(ctx context.Context, multiContent []chat.MessagePart, mc modelinfo.ModelCapabilities) []openai.ChatCompletionContentPartUnionParam {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(multiContent))
	for _, part := range multiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			parts = append(parts, openai.TextContentPart(part.Text))
		case chat.MessagePartTypeImageURL:
			// Note: superseded by MessagePartTypeDocument.
			if part.ImageURL != nil {
				parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
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

func convertMessagesWithCaps(ctx context.Context, messages []chat.Message, mc modelinfo.ModelCapabilities) []openai.ChatCompletionMessageParamUnion {
	openaiMessages := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for i := range messages {
		msg := &messages[i]

		// Skip invalid assistant messages upfront. This can happen if the model is out of tokens (max_tokens reached)
		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) == 0 && len(msg.MultiContent) == 0 && strings.TrimSpace(msg.Content) == "" && strings.TrimSpace(msg.ReasoningContent) == "" {
			continue
		}

		var openaiMessage openai.ChatCompletionMessageParamUnion

		switch msg.Role {
		case chat.MessageRoleSystem:
			if len(msg.MultiContent) == 0 {
				openaiMessage = openai.SystemMessage(msg.Content)
			} else {
				// Convert multi-content for system messages
				textParts := make([]openai.ChatCompletionContentPartTextParam, 0)
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						textParts = append(textParts, openai.ChatCompletionContentPartTextParam{
							Text: part.Text,
						})
					}
				}
				openaiMessage = openai.SystemMessage(textParts)
			}

		case chat.MessageRoleUser:
			if len(msg.MultiContent) == 0 {
				openaiMessage = openai.UserMessage(msg.Content)
			} else {
				openaiMessage = openai.UserMessage(convertMultiContentWithCaps(ctx, msg.MultiContent, mc))
			}

		case chat.MessageRoleAssistant:
			assistantParam := openai.ChatCompletionAssistantMessageParam{}
			if msg.ReasoningContent != "" {
				assistantParam.SetExtraFields(map[string]any{
					"reasoning_content": msg.ReasoningContent,
				})
			}

			if len(msg.MultiContent) == 0 {
				if msg.Content != "" {
					assistantParam.Content.OfString = param.NewOpt(msg.Content)
				}
			} else {
				// Convert multi-content for assistant messages
				contentParts := make([]openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion, 0)
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						contentParts = append(contentParts, openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion{
							OfText: &openai.ChatCompletionContentPartTextParam{
								Text: part.Text,
							},
						})
					}
				}
				if len(contentParts) > 0 {
					assistantParam.Content.OfArrayOfContentParts = contentParts
				}
			}

			if msg.FunctionCall != nil {
				assistantParam.FunctionCall.Name = msg.FunctionCall.Name
				assistantParam.FunctionCall.Arguments = msg.FunctionCall.Arguments
			}

			if len(msg.ToolCalls) > 0 {
				toolCalls := make([]openai.ChatCompletionMessageToolCallUnionParam, len(msg.ToolCalls))
				for j, toolCall := range msg.ToolCalls {
					toolCalls[j] = openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: toolCall.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      toolCall.Function.Name,
								Arguments: toolCall.Function.Arguments,
							},
						},
					}
				}
				assistantParam.ToolCalls = toolCalls
			}

			openaiMessage.OfAssistant = &assistantParam

		case chat.MessageRoleTool:
			toolParam := openai.ChatCompletionToolMessageParam{
				ToolCallID: msg.ToolCallID,
			}

			if len(msg.MultiContent) == 0 {
				toolParam.Content.OfString = param.NewOpt(msg.Content)
			} else {
				// Convert multi-content for tool messages — only text parts go in the tool message
				textParts := make([]openai.ChatCompletionContentPartTextParam, 0)
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						textParts = append(textParts, openai.ChatCompletionContentPartTextParam{
							Text: part.Text,
						})
					}
				}
				toolParam.Content.OfArrayOfContentParts = textParts
			}

			openaiMessage.OfTool = &toolParam
		}

		openaiMessages = append(openaiMessages, openaiMessage)

		// For tool messages with attachments, inject a follow-up user message
		// since OpenAI tool messages only support text.
		if msg.Role == chat.MessageRoleTool && len(msg.MultiContent) > 0 {
			var attachmentParts []openai.ChatCompletionContentPartUnionParam
			for _, part := range msg.MultiContent {
				switch part.Type {
				case chat.MessagePartTypeImageURL:
					if part.ImageURL != nil {
						attachmentParts = append(attachmentParts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
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
			if len(attachmentParts) > 0 {
				label := openai.TextContentPart("Attached content from tool result:")
				allParts := append([]openai.ChatCompletionContentPartUnionParam{label}, attachmentParts...)
				openaiMessages = append(openaiMessages, openai.UserMessage(allParts))
			}
		}
	}
	return openaiMessages
}

// getMessageRole returns the role of a message as a string.
// Returns empty string if role cannot be determined.
func getMessageRole(msg openai.ChatCompletionMessageParamUnion) string {
	if msg.OfSystem != nil {
		return "system"
	}
	if msg.OfUser != nil {
		return "user"
	}
	if msg.OfAssistant != nil {
		return "assistant"
	}
	if msg.OfTool != nil {
		return "tool"
	}
	return ""
}

// getStringContent extracts string content from a message, if available.
// Returns empty string if content is multi-part or not a string.
func getStringContent(msg openai.ChatCompletionMessageParamUnion) (string, bool) {
	if msg.OfSystem != nil {
		if str := msg.OfSystem.Content.OfString.Value; str != "" {
			return str, true
		}
	}
	if msg.OfUser != nil {
		if str := msg.OfUser.Content.OfString.Value; str != "" {
			return str, true
		}
	}
	return "", false
}

// getMultiContent extracts multi-part content from a message, if available.
func getMultiContent(msg openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionContentPartUnionParam {
	if msg.OfUser != nil && len(msg.OfUser.Content.OfArrayOfContentParts) > 0 {
		return msg.OfUser.Content.OfArrayOfContentParts
	}
	return nil
}

// getSystemTextParts extracts text parts from a system message.
func getSystemTextParts(msg openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionContentPartTextParam {
	if msg.OfSystem != nil && len(msg.OfSystem.Content.OfArrayOfContentParts) > 0 {
		return msg.OfSystem.Content.OfArrayOfContentParts
	}
	return nil
}

// MergeConsecutiveMessages merges consecutive system or user messages into single messages.
// This is needed by some local models (like those run by DMR) that don't handle
// consecutive same-role messages well.
func MergeConsecutiveMessages(openaiMessages []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	var mergedMessages []openai.ChatCompletionMessageParamUnion

	for i := 0; i < len(openaiMessages); i++ {
		currentMsg := openaiMessages[i]
		currentRole := getMessageRole(currentMsg)

		// Only merge system or user messages
		if currentRole == "system" || currentRole == "user" {
			var mergedContent strings.Builder
			var mergedMultiContent []openai.ChatCompletionContentPartUnionParam
			j := i

			// Collect all consecutive messages with the same role
			for j < len(openaiMessages) {
				msgToMerge := openaiMessages[j]
				msgRole := getMessageRole(msgToMerge)
				if msgRole != currentRole {
					break
				}

				// Extract content
				if str, ok := getStringContent(msgToMerge); ok {
					if mergedContent.Len() > 0 {
						mergedContent.WriteString("\n")
					}
					mergedContent.WriteString(str)
				} else if parts := getMultiContent(msgToMerge); parts != nil {
					mergedMultiContent = append(mergedMultiContent, parts...)
				} else if textParts := getSystemTextParts(msgToMerge); textParts != nil {
					// Convert text parts to union params
					for _, textPart := range textParts {
						mergedMultiContent = append(mergedMultiContent, openai.ChatCompletionContentPartUnionParam{
							OfText: &openai.ChatCompletionContentPartTextParam{
								Text: textPart.Text,
							},
						})
					}
				}
				j++
			}

			// Create the merged message
			var mergedMessage openai.ChatCompletionMessageParamUnion
			if currentRole == "system" {
				if len(mergedMultiContent) == 0 {
					mergedMessage = openai.SystemMessage(mergedContent.String())
				} else {
					textParts := make([]openai.ChatCompletionContentPartTextParam, 0)
					for _, part := range mergedMultiContent {
						if part.OfText != nil {
							textParts = append(textParts, *part.OfText)
						}
					}
					mergedMessage = openai.SystemMessage(textParts)
				}
			} else {
				if len(mergedMultiContent) == 0 {
					mergedMessage = openai.UserMessage(mergedContent.String())
				} else {
					mergedMessage = openai.UserMessage(mergedMultiContent)
				}
			}

			mergedMessages = append(mergedMessages, mergedMessage)
			i = j - 1
		} else {
			mergedMessages = append(mergedMessages, currentMsg)
		}
	}

	return mergedMessages
}
