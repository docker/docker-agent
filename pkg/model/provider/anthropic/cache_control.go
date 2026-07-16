package anthropic

import (
	"slices"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

const maxCacheControlBreakpoints = 4

// applyPromptCacheControl is the single policy point for cache breakpoints in
// standard Anthropic requests. Conversion functions deliberately leave cache
// control unset so a complete request cannot accumulate markers implicitly.
func applyPromptCacheControl(source []chat.Message, requestTools []tools.Tool, params *anthropic.MessageNewParams) {
	remaining := maxCacheControlBreakpoints
	for _, index := range systemCacheBreakpoints(source) {
		if remaining == 0 {
			return
		}
		if index < len(params.System) {
			params.System[index].CacheControl = anthropic.NewCacheControlEphemeralParam()
			remaining--
		}
	}

	if index, ok := toolCacheBreakpoint(requestTools); ok && remaining > 0 && index < len(params.Tools) && params.Tools[index].OfTool != nil {
		params.Tools[index].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
		remaining--
	}

	for i, marked := len(params.Messages)-1, 0; i >= 0 && remaining > 0 && marked < 2; i-- {
		if setMessageCacheControl(&params.Messages[i]) {
			remaining--
			marked++
		}
	}
}

// applyBetaPromptCacheControl mirrors applyPromptCacheControl for the SDK's
// beta request types. Keep all beta marker translation here rather than in the
// individual system, message, and tool converters.
func applyBetaPromptCacheControl(source []chat.Message, requestTools []tools.Tool, params *anthropic.BetaMessageNewParams) {
	remaining := maxCacheControlBreakpoints
	for _, index := range systemCacheBreakpoints(source) {
		if remaining == 0 {
			return
		}
		if index < len(params.System) {
			params.System[index].CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
			remaining--
		}
	}

	if index, ok := toolCacheBreakpoint(requestTools); ok && remaining > 0 && index < len(params.Tools) && params.Tools[index].OfTool != nil {
		params.Tools[index].OfTool.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
		remaining--
	}

	for i, marked := len(params.Messages)-1, 0; i >= 0 && remaining > 0 && marked < 2; i-- {
		if setBetaMessageCacheControl(&params.Messages[i]) {
			remaining--
			marked++
		}
	}
}

func systemCacheBreakpoints(messages []chat.Message) []int {
	lastBySection := make(map[chat.PromptSection]int)
	blockCount := 0
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleSystem {
			continue
		}

		before := blockCount
		if len(msg.MultiContent) > 0 {
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeText && strings.TrimSpace(part.Text) != "" {
					blockCount++
				}
			}
		} else if strings.TrimSpace(msg.Content) != "" {
			blockCount++
		}
		if msg.PromptSection != "" && blockCount > before {
			lastBySection[msg.PromptSection] = blockCount - 1
		}
	}

	indices := make([]int, 0, len(lastBySection))
	for _, index := range lastBySection {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	return indices
}

func toolCacheBreakpoint(requestTools []tools.Tool) (int, bool) {
	if !containsDeferredTool(requestTools) {
		return 0, false
	}
	immediateCount := 0
	for _, tool := range requestTools {
		if !tool.Deferred {
			immediateCount++
		}
	}
	if immediateCount == 0 {
		return len(requestTools) - 1, len(requestTools) > 0
	}
	return immediateCount - 1, true
}

func setMessageCacheControl(msg *anthropic.MessageParam) bool {
	if len(msg.Content) == 0 {
		return false
	}
	block := &msg.Content[len(msg.Content)-1]
	cacheControl := anthropic.NewCacheControlEphemeralParam()
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cacheControl
	case block.OfToolUse != nil:
		block.OfToolUse.CacheControl = cacheControl
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cacheControl
	case block.OfImage != nil:
		block.OfImage.CacheControl = cacheControl
	case block.OfDocument != nil:
		block.OfDocument.CacheControl = cacheControl
	default:
		return false
	}
	return true
}

func setBetaMessageCacheControl(msg *anthropic.BetaMessageParam) bool {
	if len(msg.Content) == 0 {
		return false
	}
	block := &msg.Content[len(msg.Content)-1]
	cacheControl := anthropic.NewBetaCacheControlEphemeralParam()
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cacheControl
	case block.OfToolUse != nil:
		block.OfToolUse.CacheControl = cacheControl
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cacheControl
	case block.OfImage != nil:
		block.OfImage.CacheControl = cacheControl
	case block.OfDocument != nil:
		block.OfDocument.CacheControl = cacheControl
	default:
		return false
	}
	return true
}
