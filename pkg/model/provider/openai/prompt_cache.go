package openai

import (
	"context"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

func sendsExplicitCacheBreakpoints(cfg *latest.ModelConfig, openAIVendor bool) bool {
	if !modelinfo.OpenAISupportsExplicitPromptCache(cfg.Model) {
		return false
	}
	if cfg.Provider == "openai" && !catalog.IsDefaultEndpoint(cfg.Provider, cfg.BaseURL) {
		return false
	}
	return modelinfo.IsOpenAIVendor(cfg.Provider, cfg.Model) || openAIVendor
}

func (c *Client) convertMessagesWithCacheBreakpoints(ctx context.Context, messages []chat.Message) []oaistream.ChatMessage {
	mc := modelinfo.ResolveCaps(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), c.CapsOverride())
	return oaistream.ConvertMessagesWithTransform(ctx, messages, mc, func(i int, converted []oaistream.ChatMessage) {
		if messages[i].CacheControl {
			markChatCacheBreakpoint(converted)
		}
	})
}

func markChatCacheBreakpoint(msgs []oaistream.ChatMessage) bool {
	for i := range slices.Backward(msgs) {
		if markChatMessageCacheBreakpoint(&msgs[i]) {
			return true
		}
	}
	return false
}

func markChatMessageCacheBreakpoint(msg *oaistream.ChatMessage) bool {
	if len(msg.ContentParts) == 0 {
		if msg.Content == nil {
			return false
		}
		msg.ContentParts = []oaistream.ContentPart{oaistream.TextContentPart(*msg.Content)}
		msg.Content = nil
	}
	for i := range slices.Backward(msg.ContentParts) {
		part := &msg.ContentParts[i]
		if msg.Role == "assistant" && part.Type != "text" {
			continue
		}
		if part.ExtraFields == nil {
			part.ExtraFields = make(map[string]any, 1)
		}
		part.ExtraFields["prompt_cache_breakpoint"] = map[string]any{"mode": "explicit"}
		return true
	}
	return false
}

func markResponseCacheBreakpoint(items []ResponseInputItem) bool {
	for i := range slices.Backward(items) {
		if markResponseInputItem(&items[i]) {
			return true
		}
	}
	return false
}

func markResponseInputItem(item *ResponseInputItem) bool {
	if len(item.Raw) > 0 {
		return false
	}
	switch item.Type {
	case "", "message":
		if item.Role == "assistant" {
			return false
		}
		if len(item.ContentParts) == 0 && item.Content != nil {
			item.ContentParts = []ResponseContentPart{{Type: "input_text", Text: *item.Content}}
			item.Content = nil
		}
		return markLastResponseContentPart(item.ContentParts)

	case "function_call_output":
		if len(item.OutputArr) == 0 && item.Output != nil {
			item.OutputArr = []ResponseContentPart{{Type: "input_text", Text: *item.Output}}
			item.Output = nil
		}
		return markLastResponseContentPart(item.OutputArr)
	}
	return false
}

func markLastResponseContentPart(parts []ResponseContentPart) bool {
	for i := range slices.Backward(parts) {
		switch parts[i].Type {
		case "input_text", "input_image", "input_file":
			parts[i].PromptCacheBreakpoint = newExplicitPromptCacheBreakpoint()
			return true
		}
	}
	return false
}
