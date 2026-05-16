package bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// convertMessages handles Bedrock's Converse API constraints:
// - Tool results must immediately follow the assistant message with tool_use
// - Multiple consecutive tool results must be grouped into a single user message
func convertMessages(ctx context.Context, messages []chat.Message, id modelsdev.ID, store *modelsdev.Store, enableCaching bool) ([]types.Message, []types.SystemContentBlock) {
	var bedrockMessages []types.Message
	var systemBlocks []types.SystemContentBlock

	for i := 0; i < len(messages); i++ {
		msg := &messages[i]

		switch msg.Role {
		case chat.MessageRoleSystem:
			// Extract system messages into separate system blocks
			if len(msg.MultiContent) > 0 {
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{
							Value: part.Text,
						})
					}
				}
			} else {
				systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberText{
					Value: msg.Content,
				})
			}

		case chat.MessageRoleUser:
			contentBlocks := convertUserContent(ctx, msg, id, store)
			if len(contentBlocks) > 0 {
				bedrockMessages = append(bedrockMessages, types.Message{
					Role:    types.ConversationRoleUser,
					Content: contentBlocks,
				})
			}

		case chat.MessageRoleAssistant:
			contentBlocks := convertAssistantContent(msg)
			if len(contentBlocks) > 0 {
				bedrockMessages = append(bedrockMessages, types.Message{
					Role:    types.ConversationRoleAssistant,
					Content: contentBlocks,
				})
			}

		case chat.MessageRoleTool:
			// Group consecutive tool results into a single user message
			// This satisfies Bedrock's requirement that tool results immediately follow
			// the assistant message with tool_use blocks
			var toolResultBlocks []types.ContentBlock
			j := i
			for j < len(messages) && messages[j].Role == chat.MessageRoleTool {
				if messages[j].ToolCallID != "" {
					toolResultBlocks = append(toolResultBlocks, &types.ContentBlockMemberToolResult{
						Value: types.ToolResultBlock{
							ToolUseId: aws.String(messages[j].ToolCallID),
							Content: []types.ToolResultContentBlock{
								&types.ToolResultContentBlockMemberText{
									Value: messages[j].Content,
								},
							},
						},
					})
				}
				j++
			}
			if len(toolResultBlocks) > 0 {
				bedrockMessages = append(bedrockMessages, types.Message{
					Role:    types.ConversationRoleUser,
					Content: toolResultBlocks,
				})
			}
			// Skip the messages we already processed
			i = j - 1
		}
	}

	// Cache points after system and on last 2 messages optimize multi-turn conversations:
	// the stable prefix (system + tools + older context) gets cached while recent messages change.
	if enableCaching {
		if len(systemBlocks) > 0 {
			systemBlocks = append(systemBlocks, &types.SystemContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
		}
		applyCachePointsToMessages(bedrockMessages)
	}

	return bedrockMessages, systemBlocks
}

func applyCachePointsToMessages(messages []types.Message) {
	// Add cache points to the last 2 messages (or all if fewer exist)
	start := max(0, len(messages)-2)
	for i := len(messages) - 1; i >= start; i-- {
		msg := &messages[i]
		if len(msg.Content) == 0 {
			continue
		}
		msg.Content = append(msg.Content, &types.ContentBlockMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
	}
}

func convertUserContent(ctx context.Context, msg *chat.Message, id modelsdev.ID, store *modelsdev.Store) []types.ContentBlock {
	var blocks []types.ContentBlock

	if len(msg.MultiContent) > 0 {
		for _, part := range msg.MultiContent {
			switch part.Type {
			case chat.MessagePartTypeText:
				blocks = append(blocks, &types.ContentBlockMemberText{
					Value: part.Text,
				})
			case chat.MessagePartTypeImageURL:
				// Note: superseded by MessagePartTypeDocument.
				if part.ImageURL != nil {
					if imageBlock := convertImageURL(part.ImageURL); imageBlock != nil {
						blocks = append(blocks, imageBlock)
					}
				}
			case chat.MessagePartTypeDocument:
				if part.Document != nil {
					docBlocks, err := convertDocument(ctx, *part.Document, id, store)
					if err != nil {
						slog.WarnContext(ctx, "failed to convert document attachment", "error", err, "doc", part.Document.Name)
						continue
					}
					blocks = append(blocks, docBlocks...)
				}
			}
		}
	} else {
		blocks = append(blocks, &types.ContentBlockMemberText{
			Value: msg.Content,
		})
	}

	return blocks
}

func convertImageURL(imageURL *chat.MessageImageURL) types.ContentBlock {
	if !strings.HasPrefix(imageURL.URL, "data:") {
		return nil
	}

	parts := strings.SplitN(imageURL.URL, ",", 2)
	if len(parts) != 2 {
		return nil
	}

	// Decode base64 data
	imageData, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}

	// Determine format from media type
	var format types.ImageFormat
	switch {
	case strings.Contains(parts[0], "image/jpeg"):
		format = types.ImageFormatJpeg
	case strings.Contains(parts[0], "image/png"):
		format = types.ImageFormatPng
	case strings.Contains(parts[0], "image/gif"):
		format = types.ImageFormatGif
	case strings.Contains(parts[0], "image/webp"):
		format = types.ImageFormatWebp
	default:
		format = types.ImageFormatJpeg
	}

	return &types.ContentBlockMemberImage{
		Value: types.ImageBlock{
			Format: format,
			Source: &types.ImageSourceMemberBytes{
				Value: imageData,
			},
		},
	}
}

func convertAssistantContent(msg *chat.Message) []types.ContentBlock {
	var blocks []types.ContentBlock

	// Add thinking blocks first (order: thinking, text, tool_use)
	if msg.ReasoningContent != "" && msg.ThinkingSignature != "" {
		blocks = append(blocks, &types.ContentBlockMemberReasoningContent{
			Value: &types.ReasoningContentBlockMemberReasoningText{
				Value: types.ReasoningTextBlock{
					Text:      aws.String(msg.ReasoningContent),
					Signature: aws.String(msg.ThinkingSignature),
				},
			},
		})
	} else if msg.ThinkingSignature != "" {
		// Redacted thinking block (signature only)
		blocks = append(blocks, &types.ContentBlockMemberReasoningContent{
			Value: &types.ReasoningContentBlockMemberRedactedContent{
				Value: []byte(msg.ThinkingSignature),
			},
		})
	}

	// Add text content if present
	if msg.Content != "" {
		blocks = append(blocks, &types.ContentBlockMemberText{
			Value: msg.Content,
		})
	}

	// Add tool use blocks for tool calls
	for _, tc := range msg.ToolCalls {
		var input map[string]any
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		}
		if input == nil {
			input = make(map[string]any)
		}

		// Convert input map to document (required by Bedrock)
		inputDoc := mapToDocument(input)

		blocks = append(blocks, &types.ContentBlockMemberToolUse{
			Value: types.ToolUseBlock{
				ToolUseId: aws.String(tc.ID),
				Name:      aws.String(tc.Function.Name),
				Input:     inputDoc,
			},
		})
	}

	return blocks
}

func mapToDocument(m map[string]any) document.Interface {
	return document.NewLazyDocument(m)
}

func convertToolConfig(requestTools []tools.Tool, enableCaching bool) *types.ToolConfiguration {
	if len(requestTools) == 0 {
		return nil
	}

	toolSpecs := make([]types.Tool, 0, len(requestTools)+1)
	for _, tool := range requestTools {
		schema := convertToolSchema(tool.Parameters)
		toolSpecs = append(toolSpecs, &types.ToolMemberToolSpec{
			Value: types.ToolSpecification{
				Name:        aws.String(tool.Name),
				Description: aws.String(tool.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{
					Value: schema,
				},
			},
		})
	}

	// Cache point after tools: tool definitions remain stable across conversation turns
	if enableCaching {
		toolSpecs = append(toolSpecs, &types.ToolMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
	}

	return &types.ToolConfiguration{
		Tools:      toolSpecs,
		ToolChoice: &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}},
	}
}

func convertToolSchema(params any) document.Interface {
	schema, err := tools.SchemaToMap(params)
	if err != nil {
		slog.Debug("Bedrock tool schema conversion failed, using empty schema", "error", err)
		schema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return document.NewLazyDocument(schema)
}
