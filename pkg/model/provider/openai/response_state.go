package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/tools"
)

func (c *Client) firstPartyResponses() bool {
	if c.ModelConfig.Provider == chatgpt.ProviderName {
		return c.ModelConfig.BaseURL == "" || strings.TrimRight(c.ModelConfig.BaseURL, "/") == chatgpt.BaseURL
	}
	return c.ModelConfig.Provider == "openai" && catalog.IsDefaultEndpoint(c.ModelConfig.Provider, c.Endpoint())
}

func (c *Client) preservesResponseState() bool {
	enabled, _ := c.ModelConfig.ProviderOpts["preserve_reasoning"].(bool)
	return c.firstPartyResponses() && (enabled || c.NativeToolSearchEnabled())
}

func (c *Client) responseSource() string {
	return c.ModelConfig.Provider + "/" + c.ModelConfig.Model
}

func (c *Client) cacheDiagnosticsEnabled() bool {
	enabled, _ := c.ModelConfig.ProviderOpts["cache_diagnostics"].(bool)
	return enabled && c.ModelConfig.Provider == "openai" && c.firstPartyResponses() &&
		modelinfo.OpenAISupportsExplicitPromptCache(c.ModelConfig.Model)
}

func (c *Client) configureResponseState(ctx context.Context, params *ResponseRequest, messages []chat.Message) {
	if c.preservesResponseState() && modelinfo.UsesReasoningEffort(c.ModelConfig.Model) {
		params.Include = append(params.Include, "reasoning.encrypted_content")
	}
	if !c.cacheDiagnosticsEnabled() {
		return
	}
	sessionID := httpclient.SessionIDFromContext(ctx)
	if sessionID == "" {
		return
	}
	for _, msg := range slices.Backward(messages) {
		if msg.Role != chat.MessageRoleAssistant {
			continue
		}
		state := msg.OpenAIResponse
		if state != nil && state.Source == c.responseSource() && state.SessionID == sessionID && state.ID != "" {
			params.PromptCacheOptions = &PromptCacheOptions{ComparisonResponseID: state.ID}
		}
		return
	}
}

func (c *Client) responseAdapter(ctx context.Context, stream responseEventStream) *ResponseStreamAdapter {
	adapter := newResponseStreamAdapter(stream, c.TrackUsageEnabled())
	if c.preservesResponseState() || c.cacheDiagnosticsEnabled() {
		adapter.responseState = &chat.OpenAIResponse{
			Source: c.responseSource(), SessionID: httpclient.SessionIDFromContext(ctx),
		}
	}
	adapter.preserveOutput = c.preservesResponseState()
	if c.cacheDiagnosticsEnabled() {
		adapter.onResponseDone = func(response ResponseObject) { logCacheDiagnostics(ctx, response) }
	}
	return adapter
}

func replayableResponseItem(item ResponseOutputItem) bool {
	switch item.Type {
	case "reasoning":
		return item.EncryptedContent != ""
	case "message":
		if item.Role != "assistant" {
			return false
		}
		return slices.ContainsFunc(item.Content, func(part ResponseOutputContent) bool {
			return part.Type == "output_text"
		})
	case "function_call":
		return item.CallID != "" && item.Name != ""
	case "tool_search_call", "tool_search_output":
		return item.Execution == "server"
	default:
		return false
	}
}

func responseItemJSON(item ResponseOutputItem) json.RawMessage {
	if len(item.Raw) > 0 {
		return item.Raw
	}
	b, _ := json.Marshal(item)
	return b
}

func (c *Client) replayResponse(msg chat.Message) []ResponseInputItem {
	state := msg.OpenAIResponse
	if !c.preservesResponseState() || state == nil || state.Source != c.responseSource() || msg.Role != chat.MessageRoleAssistant {
		return nil
	}
	var text, reasoning strings.Builder
	hasState := false
	var calls []tools.ToolCall
	input := make([]ResponseInputItem, 0, len(state.Output))
	for _, raw := range state.Output {
		var output ResponseOutputItem
		if err := json.Unmarshal(raw, &output); err != nil || !replayableResponseItem(output) {
			return nil
		}
		switch output.Type {
		case "reasoning":
			hasState = true
			for _, summary := range output.Summary {
				reasoning.WriteString(summary.Text)
			}
			for _, part := range output.Content {
				reasoning.WriteString(part.Text)
			}
		case "message":
			hasState = hasState || output.Phase != ""
			for _, part := range output.Content {
				if part.Type != "output_text" {
					return nil
				}
				text.WriteString(part.Text)
			}
		case "tool_search_call", "tool_search_output":
			hasState = true
		case "function_call":
			calls = append(calls, tools.ToolCall{ID: output.CallID, Type: "function", Function: tools.FunctionCall{
				Name: output.Name, Arguments: output.ArgumentsString(),
			}})
		}
		input = append(input, ResponseInputItem{
			Raw:       raw,
			Type:      output.Type,
			Role:      output.Role,
			CallID:    output.CallID,
			Status:    output.Status,
			Execution: output.Execution,
			Tools:     output.Tools,
		})
	}
	if reasoning.Len() > 0 && reasoning.String() != msg.ReasoningContent {
		return nil
	}
	if !hasState || text.String() != msg.Content || !slices.Equal(calls, msg.ToolCalls) || len(msg.MultiContent) != 0 {
		return nil
	}
	return input
}

func (c *Client) filterToolSearchHistory(input []ResponseInputItem, requestTools []tools.Tool) []ResponseInputItem {
	allowed := make(map[string]bool, len(requestTools))
	for _, tool := range requestTools {
		allowed[tool.Name] = true
	}
	keep := c.NativeToolSearchEnabled()
	for _, item := range input {
		if item.Type == "tool_search_output" && item.Execution == "server" {
			for _, tool := range item.Tools {
				if tool.Type != "function" || !allowed[tool.Name] {
					keep = false
				}
			}
		}
	}
	if keep {
		return input
	}
	return slices.DeleteFunc(input, func(item ResponseInputItem) bool {
		return (item.Type == "tool_search_call" || item.Type == "tool_search_output") && item.Execution == "server"
	})
}

func logCacheDiagnostics(ctx context.Context, response ResponseObject) {
	diagnostic := response.PromptCacheDiagnostics
	if diagnostic.Type == "" {
		return
	}
	slog.DebugContext(ctx, "OpenAI prompt cache diagnostics",
		"session_id", httpclient.SessionIDFromContext(ctx),
		"response_id", response.ID,
		"type", diagnostic.Type,
		"reason", diagnostic.Reason,
		"comparison_reusable_tokens", diagnostic.ComparisonReusableTokens,
		"cache_missed_tokens", diagnostic.CacheMissedTokens,
		"cached_tokens", response.Usage.InputTokensDetails.CachedTokens,
	)
}
