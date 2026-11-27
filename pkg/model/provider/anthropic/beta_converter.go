package anthropic

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func (c *Client) convertBetaMessages(ctx context.Context, messages []chat.Message) ([]MessageParam, error) {
	return c.convertBetaMessagesWithDeferred(ctx, messages, nil)
}

func (c *Client) convertBetaMessagesWithDeferred(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) ([]MessageParam, error) {
	var betaMessages []MessageParam
	deferredByCallID := deferredToolNamesByCallID(requestTools)
	loopStart := chat.ActiveToolLoopStart(messages)

	for i := 0; i < len(messages); i++ {
		msg := &messages[i]
		if compact, ok, err := c.compactionMessage(msg); err != nil {
			return nil, err
		} else if ok {
			if len(betaMessages) != 0 {
				return nil, errors.New("anthropic: signed compaction must precede conversation messages")
			}
			betaMessages = append(betaMessages, compact)
			continue
		}
		if msg.Role == chat.MessageRoleSystem {
			continue
		}
		if msg.Role == chat.MessageRoleUser {
			if len(msg.MultiContent) > 0 {
				contentBlocks, err := c.convertUserMultiContent(ctx, msg.MultiContent)
				if err != nil {
					return nil, err
				}
				if len(contentBlocks) > 0 {
					betaMessages = append(betaMessages, MessageParam{
						Role:    "user",
						Content: contentBlocks,
					})
				}
			} else {
				betaMessages = append(betaMessages, MessageParam{
					Role:    "user",
					Content: []ContentBlockParam{newTextBlock(msg.Content)},
				})
			}
			continue
		}
		if msg.Role == chat.MessageRoleAssistant {
			if blocks, ok, err := betaReplayContent(msg); err != nil {
				return nil, err
			} else if ok {
				betaMessages = append(betaMessages, MessageParam{Role: "assistant", Content: blocks})
				continue
			}
			if blocks, ok := betaRecoveredParams(msg); ok {
				betaMessages = append(betaMessages, MessageParam{Role: "assistant", Content: blocks})
				continue
			}
			contentBlocks := make([]ContentBlockParam, 0)

			if include, err := c.flattenedThinking(i, loopStart, msg); err != nil {
				return nil, err
			} else if include {
				contentBlocks = append(contentBlocks, newThinkingBlock(msg.ThinkingSignature, msg.ReasoningContent))
			}

			if msg.Content != "" {
				contentBlocks = append(contentBlocks, newTextBlock(msg.Content))
			}

			if len(msg.ToolCalls) > 0 {
				for _, toolCall := range msg.ToolCalls {
					var inpts map[string]any
					if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &inpts); err != nil {
						inpts = map[string]any{}
					}
					contentBlocks = append(contentBlocks, ContentBlockParam{
						Type:  "tool_use",
						ID:    toolCall.ID,
						Input: inpts,
						Name:  toolCall.Function.Name,
					})
				}
			}

			if len(contentBlocks) > 0 {
				betaMessages = append(betaMessages, MessageParam{
					Role:    "assistant",
					Content: contentBlocks,
				})
			}
			continue
		}
		if msg.Role == chat.MessageRoleTool {
			var toolResultBlocks []ContentBlockParam
			j := i
			for j < len(messages) && messages[j].Role == chat.MessageRoleTool {
				if names := deferredByCallID[messages[j].ToolCallID]; len(names) > 0 {
					content := make([]ContentBlockParam, 0, len(names))
					for _, name := range names {
						content = append(content, ContentBlockParam{
							Type:     "tool_reference",
							ToolName: name,
						})
					}
					toolResultBlocks = append(toolResultBlocks, ContentBlockParam{
						Type:      "tool_result",
						ToolUseID: messages[j].ToolCallID,
						IsError:   new(messages[j].IsError),
						Content:   content,
					})
					j++
					continue
				}
				tr, err := c.convertToolResultBlock(ctx, &messages[j])
				if err != nil {
					return nil, err
				}
				toolResultBlocks = append(toolResultBlocks, tr)
				j++
			}
			if len(toolResultBlocks) > 0 {
				betaMessages = append(betaMessages, MessageParam{
					Role:    "user",
					Content: toolResultBlocks,
				})
			}
			i = j - 1
			continue
		}
	}

	applyMessageCacheControl(betaMessages, messageCacheBreakpoints(requestTools))

	return betaMessages, nil
}
