package anthropic

import (
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// Anthropic allows at most 4 cache_control breakpoints per request; a
// request exceeding the limit is rejected outright. The budget is
// allocated in one place — this file — as follows:
//
//	tool list       0 or 1 (only when deferred tools are in play)
//	system blocks   up to maxSystemCacheBreakpoints, honoring upstream
//	                CacheControl marks (excess marks are dropped)
//	message tail    the remainder (messageCacheBreakpoints)
const maxSystemCacheBreakpoints = 2

func messageCacheBreakpoints(requestTools []tools.Tool) int {
	if containsDeferredTool(requestTools) {
		return 1
	}
	return 2
}

func markSystemBlockCacheControl(block *TextBlockParam, marked int) int {
	if block.CacheControl.Type != "" {
		return marked
	}
	if marked >= maxSystemCacheBreakpoints {
		slog.Debug("Dropping over-budget system cache breakpoint mark",
			"max", maxSystemCacheBreakpoints)
		return marked
	}
	block.CacheControl = *newEphemeralCacheControl()
	return marked + 1
}

// extractSystemBlocks converts any system-role messages into Anthropic system text blocks
// to be set on the top-level MessageRequest.System field.
func extractSystemBlocks(messages []chat.Message) []TextBlockParam {
	var systemBlocks []TextBlockParam
	marked := 0
	for i := range messages {
		msg := &messages[i]
		if msg.Role != chat.MessageRoleSystem {
			continue
		}

		if len(msg.MultiContent) > 0 {
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeText {
					if txt := strings.TrimSpace(part.Text); txt != "" {
						systemBlocks = append(systemBlocks, TextBlockParam{Text: txt})
					}
				}
			}
		} else if txt := strings.TrimSpace(msg.Content); txt != "" {
			// Trim system-message content: YAML literal blocks (instruction: |) always
			// append a trailing newline, and we don't want that in API payloads.
			systemBlocks = append(systemBlocks, TextBlockParam{
				Text: txt,
			})
		}

		if msg.CacheControl && len(systemBlocks) > 0 {
			marked = markSystemBlockCacheControl(&systemBlocks[len(systemBlocks)-1], marked)
		}
	}

	return systemBlocks
}

// applyMessageCacheControl adds ephemeral cache control to the last content block
// of the last `breakpoints` messages for prompt caching.
func applyMessageCacheControl(messages []MessageParam, breakpoints int) {
	for i := len(messages) - 1; i >= 0 && i >= len(messages)-breakpoints; i-- {
		content := messages[i].Content
		if len(content) == 0 {
			continue
		}
		last := &content[len(content)-1]
		if last.SupportsCacheControl() {
			last.CacheControl = newEphemeralCacheControl()
		}
	}
}
