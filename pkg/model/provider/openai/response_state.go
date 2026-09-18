package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/tools"
)

// Opaque state must not leak to a different vendor's compatible endpoint.
func (c *Client) firstPartyResponses() bool {
	if c.ModelConfig.Provider == chatgpt.ProviderName {
		return c.ModelConfig.BaseURL == "" || strings.TrimRight(c.ModelConfig.BaseURL, "/") == chatgpt.BaseURL
	}
	return c.ModelConfig.Provider == "openai" && c.ModelConfig.BaseURL == ""
}

// Native search needs ordered output replay to retain its loaded tools.
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

func (c *Client) configureResponseState(ctx context.Context, params *responses.ResponseNewParams, messages []chat.Message) {
	if c.preservesResponseState() && modelinfo.UsesReasoningEffort(c.ModelConfig.Model) {
		params.Include = append(params.Include, responses.ResponseIncludable("reasoning.encrypted_content"))
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
			params.PromptCacheOptions.ComparisonResponseID = param.NewOpt(state.ID)
		}
		// Do not compare across a model switch or a turn without response metadata.
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
		adapter.onResponseDone = func(response responses.Response) { logCacheDiagnostics(ctx, response) }
	}
	return adapter
}

func replayableResponseItem(item responses.ResponseOutputItemUnion) bool {
	switch item.Type {
	case "reasoning":
		return item.EncryptedContent != ""
	case "message":
		if item.Role != "assistant" {
			return false
		}
		return slices.ContainsFunc(item.Content, func(part responses.ResponseOutputMessageContentUnion) bool {
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

func responseItemJSON(item responses.ResponseOutputItemUnion) json.RawMessage {
	return json.RawMessage(item.RawJSON())
}

// replayResponse preserves ordering and phase, but never bypasses hook edits,
// tool-name sanitization, or a provider/model switch with stale raw content.
func (c *Client) replayResponse(msg chat.Message) []responses.ResponseInputItemUnionParam {
	state := msg.OpenAIResponse
	if !c.preservesResponseState() || state == nil || state.Source != c.responseSource() || msg.Role != chat.MessageRoleAssistant {
		return nil
	}
	var text, reasoning strings.Builder
	hasState := false
	var calls []tools.ToolCall
	input := make([]responses.ResponseInputItemUnionParam, 0, len(state.Output))
	for _, raw := range state.Output {
		var output responses.ResponseOutputItemUnion
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
				Name: output.Name, Arguments: output.Arguments.OfString,
			}})
		}
		var item responses.ResponseInputItemUnionParam
		if output.Type == "message" {
			item.OfOutputMessage = new(responses.ResponseOutputMessageParam)
			if err := json.Unmarshal(raw, item.OfOutputMessage); err != nil {
				return nil
			}
		} else if err := json.Unmarshal(raw, &item); err != nil {
			return nil
		}
		input = append(input, item)
	}
	if reasoning.Len() > 0 && reasoning.String() != msg.ReasoningContent {
		return nil
	}
	if !hasState || text.String() != msg.Content || !slices.Equal(calls, msg.ToolCalls) || len(msg.MultiContent) != 0 {
		return nil
	}
	return input
}

// Loaded tool definitions cannot outlive the current permission-filtered catalog.
func (c *Client) filterToolSearchHistory(input []responses.ResponseInputItemUnionParam, requestTools []tools.Tool) []responses.ResponseInputItemUnionParam {
	allowed := make(map[string]bool, len(requestTools))
	for _, tool := range requestTools {
		allowed[tool.Name] = true
	}
	keep := c.NativeToolSearchEnabled()
	for _, item := range input {
		if output := item.OfToolSearchOutput; output != nil && output.Execution == responses.ResponseToolSearchOutputItemParamExecutionServer {
			for _, tool := range output.Tools {
				if tool.OfFunction == nil || !allowed[tool.OfFunction.Name] {
					keep = false
				}
			}
		}
	}
	if keep {
		return input
	}
	// Drop call/output pairs together; server searches have no call_id.
	return slices.DeleteFunc(input, func(item responses.ResponseInputItemUnionParam) bool {
		return item.OfToolSearchCall != nil && item.OfToolSearchCall.Execution == "server" ||
			item.OfToolSearchOutput != nil && item.OfToolSearchOutput.Execution == responses.ResponseToolSearchOutputItemParamExecutionServer
	})
}

func logCacheDiagnostics(ctx context.Context, response responses.Response) {
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
