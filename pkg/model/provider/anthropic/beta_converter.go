package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// convertBetaMessages converts chat messages to Anthropic Beta API format
// Following Anthropic's extended thinking documentation with interleaved thinking enabled:
// - Thinking blocks can appear anywhere in the conversation (not required to be first)
// - Always include the complete, unmodified thinking block from previous assistant turns
// - interleaved parameter is kept for API compatibility but always true
//
// Important: Anthropic API requires that all tool_result blocks corresponding to tool_use
// blocks from the same assistant message MUST be grouped into a single user message.
func (c *Client) convertBetaMessages(ctx context.Context, messages []chat.Message) ([]anthropic.BetaMessageParam, error) {
	var betaMessages []anthropic.BetaMessageParam

	for i := 0; i < len(messages); i++ {
		msg := &messages[i]
		if msg.Role == chat.MessageRoleSystem {
			// System messages handled separately
			continue
		}
		if msg.Role == chat.MessageRoleUser {
			// Handle user messages (including images, files, and tool results)
			if len(msg.MultiContent) > 0 {
				contentBlocks, err := c.convertBetaUserMultiContent(ctx, msg.MultiContent)
				if err != nil {
					return nil, err
				}
				if len(contentBlocks) > 0 {
					betaMessages = append(betaMessages, anthropic.BetaMessageParam{
						Role:    anthropic.BetaMessageParamRoleUser,
						Content: contentBlocks,
					})
				}
			} else {
				betaMessages = append(betaMessages, anthropic.BetaMessageParam{
					Role: anthropic.BetaMessageParamRoleUser,
					Content: []anthropic.BetaContentBlockParamUnion{
						{OfText: &anthropic.BetaTextBlockParam{Text: msg.Content}},
					},
				})
			}
			continue
		}
		if msg.Role == chat.MessageRoleAssistant {
			contentBlocks := make([]anthropic.BetaContentBlockParamUnion, 0)

			// With interleaved thinking, we can include thinking blocks anywhere
			// If we have thinking content, include it first (conventional order)
			if msg.ReasoningContent != "" && msg.ThinkingSignature != "" {
				contentBlocks = append(contentBlocks,
					anthropic.NewBetaThinkingBlock(msg.ThinkingSignature, msg.ReasoningContent))
			} else if msg.ThinkingSignature != "" {
				// Include redacted thinking placeholder using the original signature
				contentBlocks = append(contentBlocks,
					anthropic.NewBetaRedactedThinkingBlock(msg.ThinkingSignature))
			}

			// Add text content if present
			if msg.Content != "" {
				contentBlocks = append(contentBlocks, anthropic.BetaContentBlockParamUnion{
					OfText: &anthropic.BetaTextBlockParam{Text: msg.Content},
				})
			}

			// Add tool calls
			if len(msg.ToolCalls) > 0 {
				for _, toolCall := range msg.ToolCalls {
					var inpts map[string]any
					if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &inpts); err != nil {
						inpts = map[string]any{}
					}
					contentBlocks = append(contentBlocks, anthropic.BetaContentBlockParamUnion{
						OfToolUse: &anthropic.BetaToolUseBlockParam{
							ID:    toolCall.ID,
							Input: inpts,
							Name:  toolCall.Function.Name,
						},
					})
				}
			}

			if len(contentBlocks) > 0 {
				betaMessages = append(betaMessages, anthropic.BetaMessageParam{
					Role:    anthropic.BetaMessageParamRoleAssistant,
					Content: contentBlocks,
				})
			}
			continue
		}
		if msg.Role == chat.MessageRoleTool {
			// Collect consecutive tool messages and merge them into a single user message
			// This is required by Anthropic API: all tool_result blocks for tool_use blocks
			// from the same assistant message must be in the same user message
			toolResultBlocks := []anthropic.BetaContentBlockParamUnion{
				convertBetaToolResultBlock(msg),
			}

			// Look ahead for consecutive tool messages and merge them
			j := i + 1
			for j < len(messages) && messages[j].Role == chat.MessageRoleTool {
				toolResultBlocks = append(toolResultBlocks, convertBetaToolResultBlock(&messages[j]))
				j++
			}

			// Add the merged user message with all tool results
			betaMessages = append(betaMessages, anthropic.BetaMessageParam{
				Role:    anthropic.BetaMessageParamRoleUser,
				Content: toolResultBlocks,
			})

			// Skip the messages we've already processed
			i = j - 1
			continue
		}
	}

	// Add ephemeral cache to last 2 messages' last content block
	applyBetaMessageCacheControl(betaMessages)

	return betaMessages, nil
}

// convertBetaUserMultiContent converts user message multi-content parts to Beta API content blocks.
// It handles text, images (base64 and URL), and file uploads via the Files API.
// convertBetaToolResultBlock converts a tool message to a Beta API tool_result block,
// including any image content from MultiContent.
func convertBetaToolResultBlock(msg *chat.Message) anthropic.BetaContentBlockParamUnion {
	if !hasImageMultiContent(msg.MultiContent) {
		// tool_result must be present for every preceding tool_use; we cannot skip
		// it. Normalize whitespace-only content to empty string rather than skipping.
		content := msg.Content
		if strings.TrimSpace(content) == "" {
			content = ""
		}
		return anthropic.BetaContentBlockParamUnion{
			OfToolResult: &anthropic.BetaToolResultBlockParam{
				ToolUseID: msg.ToolCallID,
				Content: []anthropic.BetaToolResultBlockParamContentUnion{
					{OfText: &anthropic.BetaTextBlockParam{Text: content}},
				},
			},
		}
	}

	var content []anthropic.BetaToolResultBlockParamContentUnion
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			content = append(content, anthropic.BetaToolResultBlockParamContentUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: part.Text},
			})
		case chat.MessagePartTypeImageURL:
			// Note: superseded by MessagePartTypeDocument.
			if part.ImageURL == nil {
				continue
			}
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
				if len(urlParts) == 2 {
					mediaType := extractMediaType(urlParts[0])
					content = append(content, anthropic.BetaToolResultBlockParamContentUnion{
						OfImage: &anthropic.BetaImageBlockParam{
							Source: anthropic.BetaImageBlockParamSourceUnion{
								OfBase64: &anthropic.BetaBase64ImageSourceParam{
									Data:      urlParts[1],
									MediaType: anthropic.BetaBase64ImageSourceMediaType(mediaType),
								},
							},
						},
					})
				}
			}
		}
	}

	return anthropic.BetaContentBlockParamUnion{
		OfToolResult: &anthropic.BetaToolResultBlockParam{
			ToolUseID: msg.ToolCallID,
			Content:   content,
		},
	}
}

func (c *Client) convertBetaUserMultiContent(ctx context.Context, parts []chat.MessagePart) ([]anthropic.BetaContentBlockParamUnion, error) {
	contentBlocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(parts))

	for _, part := range parts {
		switch part.Type {
		case chat.MessagePartTypeText:
			contentBlocks = append(contentBlocks, anthropic.BetaContentBlockParamUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: part.Text},
			})

		case chat.MessagePartTypeImageURL:
			if part.ImageURL == nil {
				continue
			}
			// Handle base64 data URLs (legacy format)
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
				if len(urlParts) == 2 {
					mediaType := extractMediaType(urlParts[0])
					base64Data := urlParts[1]

					contentBlocks = append(contentBlocks, anthropic.BetaContentBlockParamUnion{
						OfImage: &anthropic.BetaImageBlockParam{
							Source: anthropic.BetaImageBlockParamSourceUnion{
								OfBase64: &anthropic.BetaBase64ImageSourceParam{
									Data:      base64Data,
									MediaType: anthropic.BetaBase64ImageSourceMediaType(mediaType),
								},
							},
						},
					})
				}
			} else if strings.HasPrefix(part.ImageURL.URL, "http://") || strings.HasPrefix(part.ImageURL.URL, "https://") {
				// URL-based images
				contentBlocks = append(contentBlocks, anthropic.BetaContentBlockParamUnion{
					OfImage: &anthropic.BetaImageBlockParam{
						Source: anthropic.BetaImageBlockParamSourceUnion{
							OfURL: &anthropic.BetaURLImageSourceParam{
								URL: part.ImageURL.URL,
							},
						},
					},
				})
			}

		case chat.MessagePartTypeFile:
			// Note: superseded by MessagePartTypeDocument.
			if part.File == nil {
				continue
			}

			switch {
			case part.File.Path != "" && part.File.FileID == "":
				// Upload the file if we have a path but no file ID
				if c.fileManager == nil {
					return nil, fmt.Errorf("%w: cannot upload file %s", ErrFileManagerNotInitialized, part.File.Path)
				}

				uploaded, err := c.fileManager.GetOrUpload(ctx, part.File.Path)
				if err != nil {
					return nil, fmt.Errorf("failed to upload file %s: %w", part.File.Path, err)
				}

				block, err := createBetaFileContentBlock(uploaded.FileID, uploaded.MimeType)
				if err != nil {
					return nil, err
				}
				contentBlocks = append(contentBlocks, block)

			case part.File.FileID != "":
				// File already uploaded, use the ID directly
				block, err := createBetaFileContentBlock(part.File.FileID, part.File.MimeType)
				if err != nil {
					return nil, err
				}
				contentBlocks = append(contentBlocks, block)

			default:
				// File part has neither path nor file ID - this is invalid
				return nil, errors.New("invalid file attachment: neither path nor file_id provided")
			}

		case chat.MessagePartTypeDocument:
			if part.Document != nil {
				stdBlocks, err := c.convertDoc(ctx, *part.Document)
				if err != nil {
					return nil, fmt.Errorf("failed to convert document attachment %q: %w", part.Document.Name, err)
				}
				contentBlocks = append(contentBlocks, stdBlocksToBeta(stdBlocks)...)
			}
		}
	}

	return contentBlocks, nil
}

// createBetaFileContentBlock creates the appropriate Beta API content block for a file.
func createBetaFileContentBlock(fileID, mimeType string) (anthropic.BetaContentBlockParamUnion, error) {
	if IsImageMime(mimeType) {
		return anthropic.BetaContentBlockParamUnion{
			OfImage: &anthropic.BetaImageBlockParam{
				Source: anthropic.BetaImageBlockParamSourceUnion{
					OfFile: &anthropic.BetaFileImageSourceParam{
						FileID: fileID,
					},
				},
			},
		}, nil
	}

	if IsAnthropicDocumentMime(mimeType) {
		return anthropic.BetaContentBlockParamUnion{
			OfDocument: &anthropic.BetaRequestDocumentBlockParam{
				Source: anthropic.BetaRequestDocumentBlockSourceUnionParam{
					OfFile: &anthropic.BetaFileDocumentSourceParam{
						FileID: fileID,
					},
				},
			},
		}, nil
	}

	return anthropic.BetaContentBlockParamUnion{}, fmt.Errorf("%w: %s", ErrUnsupportedFileType, mimeType)
}

// extractBetaSystemBlocks extracts system messages for Beta API format
func extractBetaSystemBlocks(messages []chat.Message) []anthropic.BetaTextBlockParam {
	regularBlocks := extractSystemBlocks(messages)

	betaBlocks := make([]anthropic.BetaTextBlockParam, len(regularBlocks))
	for i, block := range regularBlocks {
		betaBlocks[i] = anthropic.BetaTextBlockParam{Text: block.Text}

		// Copy over cache control from regular blocks (already set on first 2)
		if block.CacheControl.Type != "" {
			betaBlocks[i].CacheControl = anthropic.BetaCacheControlEphemeralParam{
				Type: block.CacheControl.Type,
				TTL:  anthropic.BetaCacheControlEphemeralTTL(block.CacheControl.TTL),
			}
		}
	}

	return betaBlocks
}

// convertBetaTools converts tools to Beta API format
func convertBetaTools(t []tools.Tool) ([]anthropic.BetaToolUnionParam, error) {
	betaTools := make([]anthropic.BetaToolUnionParam, len(t))

	for i, tool := range t {
		inputSchema, err := ConvertParametersToSchema(tool.Parameters)
		if err != nil {
			return nil, err
		}

		// Convert to BetaToolInputSchemaParam
		var betaInputSchema anthropic.BetaToolInputSchemaParam
		if err := tools.ConvertSchema(inputSchema, &betaInputSchema); err != nil {
			return nil, err
		}

		// Create BetaToolParam and wrap it in BetaToolUnionParam
		betaTools[i] = anthropic.BetaToolUnionParam{
			OfTool: &anthropic.BetaToolParam{
				Name:        tool.Name,
				Description: anthropic.String(tool.Description),
				InputSchema: betaInputSchema,
			},
		}
	}

	return betaTools, nil
}

// applyBetaMessageCacheControl adds ephemeral cache control to the last content block
// of the last 2 messages for prompt caching.
func applyBetaMessageCacheControl(messages []anthropic.BetaMessageParam) {
	for i := len(messages) - 1; i >= 0 && i >= len(messages)-2; i-- {
		msg := &messages[i]
		if len(msg.Content) == 0 {
			continue
		}
		lastIdx := len(msg.Content) - 1
		block := &msg.Content[lastIdx]
		cacheCtrl := anthropic.NewBetaCacheControlEphemeralParam()
		switch {
		case block.OfText != nil:
			block.OfText.CacheControl = cacheCtrl
		case block.OfToolUse != nil:
			block.OfToolUse.CacheControl = cacheCtrl
		case block.OfToolResult != nil:
			block.OfToolResult.CacheControl = cacheCtrl
		case block.OfImage != nil:
			block.OfImage.CacheControl = cacheCtrl
		case block.OfDocument != nil:
			block.OfDocument.CacheControl = cacheCtrl
		}
	}
}

// stdBlocksToBeta converts standard Anthropic SDK content blocks to their Beta
// API equivalents. The beta path (convertBetaUserMultiContent) requires
// BetaContentBlockParamUnion, but convertDocument produces the standard type.
// The two types are structurally identical for the block kinds we produce
// (text, image, document), so we field-map them here.
func stdBlocksToBeta(blocks []anthropic.ContentBlockParamUnion) []anthropic.BetaContentBlockParamUnion {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.OfText != nil:
			out = append(out, anthropic.BetaContentBlockParamUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: b.OfText.Text},
			})
		case b.OfImage != nil && b.OfImage.Source.OfBase64 != nil:
			out = append(out, anthropic.BetaContentBlockParamUnion{
				OfImage: &anthropic.BetaImageBlockParam{
					Source: anthropic.BetaImageBlockParamSourceUnion{
						OfBase64: &anthropic.BetaBase64ImageSourceParam{
							Data:      b.OfImage.Source.OfBase64.Data,
							MediaType: anthropic.BetaBase64ImageSourceMediaType(b.OfImage.Source.OfBase64.MediaType),
						},
					},
				},
			})
		case b.OfDocument != nil && b.OfDocument.Source.OfBase64 != nil:
			out = append(out, anthropic.BetaContentBlockParamUnion{
				OfDocument: &anthropic.BetaRequestDocumentBlockParam{
					Source: anthropic.BetaRequestDocumentBlockSourceUnionParam{
						OfBase64: &anthropic.BetaBase64PDFSourceParam{
							Data: b.OfDocument.Source.OfBase64.Data,
						},
					},
				},
			})
		}
		// Unknown block kinds are dropped (should never happen in Phase 1).
	}
	return out
}
