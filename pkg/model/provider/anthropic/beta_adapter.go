package anthropic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/respjson"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/tools"
)

// betaStreamAdapter adapts the Anthropic Beta stream to our interface
type betaStreamAdapter struct {
	retryableStream[anthropic.BetaRawMessageStreamEventUnion]

	trackUsage bool
	toolCall   bool
	stopReason anthropic.BetaStopReason
	// toolIDByBlock maps a content block index to its tool_use block ID.
	// See streamAdapter.toolIDByBlock for the same rationale (parallel
	// tool calls require per-block routing of input_json deltas).
	toolIDByBlock map[int64]string
	// message/rawLost: see streamAdapter.
	message        anthropic.BetaMessage
	rawLost        bool
	requestContext json.RawMessage
	// config is the dispatching client's; served is the model a server
	// fallback handed the turn to, unset when the dispatched model served it.
	// The runtime may read it while the reader goroutine still writes it
	// (cancellation, idle timeout).
	provisional    atomic.Pointer[string]
	settled        atomic.Bool
	config         base.Config
	fallbackModels []string
	markerSeen     bool
	served         atomic.Pointer[string]
	refusals       []refusalEvidence
}

// newBetaStreamAdapter creates a new Beta stream adapter
func (c *Client) newBetaStreamAdapter(stream *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion], trackUsage bool) *betaStreamAdapter {
	return &betaStreamAdapter{
		retryableStream: retryableStream[anthropic.BetaRawMessageStreamEventUnion]{stream: stream},
		trackUsage:      trackUsage,
		toolIDByBlock:   map[int64]string{},
		config:          c.Config,
	}
}

// Recv gets the next completion chunk from the Beta stream
func (a *betaStreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	ok, err := a.next()
	if !ok {
		return chat.MessageStreamResponse{}, wrapAnthropicError(err)
	}

	event := a.stream.Current()
	a.accumulate(event)

	response := chat.MessageStreamResponse{
		ID:     event.Message.ID,
		Object: "chat.completion.chunk",
		Model:  event.Message.Model,
		Choices: []chat.MessageStreamChoice{
			{
				Index: 0,
				Delta: chat.MessageDelta{
					Role: string(chat.MessageRoleAssistant),
				},
			},
		},
	}

	// Handle different event types
	switch eventVariant := event.AsAny().(type) {
	case anthropic.BetaRawMessageStartEvent:
		model := eventVariant.Message.Model
		// Sticky routing has no marker; retain an early candidate if usage never arrives.
		if model != "" && model != a.config.ModelConfig.Model && slices.Contains(a.fallbackModels, model) {
			a.provisional.Store(&model)
		}
	case anthropic.BetaRawContentBlockStartEvent:
		switch block := eventVariant.ContentBlock.AsAny().(type) {
		case anthropic.BetaToolUseBlock:
			a.toolIDByBlock[eventVariant.Index] = block.ID
			a.toolCall = true
			toolCall := tools.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: tools.FunctionCall{
					Name: block.Name,
				},
			}
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
		case anthropic.BetaFallbackBlock:
			// The declined model's tool calls are superseded.
			a.toolCall = false
			a.markerSeen = true
			a.served.Store(&block.To.Model)
			category := block.Trigger.JSON.Category
			if block.Trigger.Type != "refusal" {
				category = respjson.Field{}
			}
			a.refusals = append(a.refusals, refusalEvidence{from: block.From.Model, to: block.To.Model, category: category})
			response.Choices[0].Delta.ServerFallback = &chat.ServerFallback{From: block.From.Model, To: block.To.Model}
		case anthropic.BetaThinkingBlock:
			if block.Thinking != "" {
				response.Choices[0].Delta.ReasoningContent = block.Thinking
				slog.Debug("Received thinking", "thinking", block.Thinking)
			}
			if block.Signature != "" {
				response.Choices[0].Delta.ThinkingSignature = block.Signature
			}
		}
	case anthropic.BetaRawContentBlockDeltaEvent:
		switch deltaVariant := eventVariant.Delta.AsAny().(type) {
		case anthropic.BetaTextDelta:
			response.Choices[0].Delta.Content = deltaVariant.Text
		case anthropic.BetaThinkingDelta:
			response.Choices[0].Delta.ReasoningContent = deltaVariant.Thinking
		case anthropic.BetaInputJSONDelta:
			inputBytes := deltaVariant.PartialJSON
			toolCall := tools.ToolCall{
				ID:   a.toolIDByBlock[eventVariant.Index],
				Type: "function",
				Function: tools.FunctionCall{
					Arguments: inputBytes,
				},
			}
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
		case anthropic.BetaSignatureDelta:
			// Signature delta is for thinking blocks - capture it so we can replay thinking in history
			response.Choices[0].Delta.ThinkingSignature = deltaVariant.Signature
		default:
			return response, fmt.Errorf("unknown delta type: %T", deltaVariant)
		}
	case anthropic.BetaRawMessageDeltaEvent:
		a.stopReason = eventVariant.Delta.StopReason
		// Confirmed usage supersedes the candidate, but never an explicit marker.
		if !a.markerSeen {
			for _, it := range slices.Backward(a.message.Usage.Iterations) {
				if it.Type == "fallback_message" {
					a.served.Store(&it.Model)
					break
				}
			}
		}
		a.settled.Store(true)
		if a.trackUsage {
			response.Usage = betaUsageFromMessage(a.message.Usage)
			terminal := a.message.StopDetails.JSON.Category
			if a.message.StopDetails.Type != "refusal" {
				terminal = respjson.Field{}
			}
			response.UsageAttempts = serverFallbackAttempts(a.message.Usage.Iterations, a.refusals, a.config.ModelConfig.Model, a.stopReason, terminal)
		}
	case anthropic.BetaRawMessageStopEvent:
		for _, dropped := range thinkingTransformations(a.message.InputTransformations) {
			slog.Warn("Anthropic dropped invalidated thinking", "block", dropped)
		}
		response.Choices[0].FinishReason = finishReason(anthropic.StopReason(a.stopReason), a.toolCall)
		if !a.rawLost {
			response.Choices[0].Delta.ProviderState = newProviderState(a.message.ID, a.message.Model, a.message.Content)
		}
		state := withRequestContext(response.Choices[0].Delta.ProviderState, a.requestContext)
		diagnosis := cacheDiagnostics(a.message.Diagnostics)
		if state == nil && diagnosis != nil {
			state = &chat.ProviderState{Provider: providerStateName, MessageID: a.message.ID, Content: json.RawMessage("[]")}
		}
		if state != nil {
			state.CacheDiagnostics = diagnosis
		}
		response.Choices[0].Delta.ProviderState = state
	}

	return response, nil
}

func (a *betaStreamAdapter) accumulate(event anthropic.BetaRawMessageStreamEventUnion) {
	// Usage must keep accumulating even if raw content capture failed.
	if a.rawLost && event.Type != "message_start" && event.Type != "message_delta" {
		return
	}
	if err := a.message.Accumulate(event); err != nil {
		a.rawLost = true
		slog.Debug("Anthropic Beta: raw response capture disabled", "error", err)
	}
}

// ServedBy reports the model a server fallback handed the turn to.
func (a *betaStreamAdapter) ServedBy() (contracts.ServingModel, bool) {
	served := a.served.Load()
	if served == nil && !a.settled.Load() {
		served = a.provisional.Load()
	}
	if served == nil {
		return contracts.ServingModel{}, false
	}
	cfg := a.config
	cfg.ModelConfig.Model = *served
	// The primary's display name and cost overrides don't describe the
	// fallback model, which is priced from the catalogue.
	cfg.ModelConfig.DisplayModel = ""
	cfg.ModelConfig.Cost = nil
	return contracts.ServingModel{ID: cfg.ID(), Config: cfg}, true
}

// Close closes the Beta stream
func (a *betaStreamAdapter) Close() {
	a.close()
}

// betaUsageFromMessage maps accumulated usage; reasoning is already included in output.
func betaUsageFromMessage(u anthropic.BetaUsage) *chat.Usage {
	return &chat.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
		ReasoningTokens:   u.OutputTokensDetails.ThinkingTokens,
	}
}

// billedBeforeOutput lists the refusal categories billed even when the model
// declined before any output, as of September 2026; Anthropic may change it.
// https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback#how-refusals-are-billed
var billedBeforeOutput = map[string]bool{"bio": true, "frontier_llm": true, "reasoning_extraction": true}

// refusalEvidence binds wire-level category presence to the model that declined.
type refusalEvidence struct {
	from, to string
	category respjson.Field
}

// serverFallbackAttempts aggregates server-tool iterations into model hops.
// Unmatched refusal evidence never exempts a hop from billing.
func serverFallbackAttempts(iterations []anthropic.BetaIterationsUsageItemUnion, refusals []refusalEvidence, requestedModel string, stopReason anthropic.BetaStopReason, terminal respjson.Field) []chat.UsageAttempt {
	if !slices.ContainsFunc(iterations, func(it anthropic.BetaIterationsUsageItemUnion) bool { return it.Type == "fallback_message" }) {
		return nil
	}
	var attempts []chat.UsageAttempt
	for _, it := range iterations {
		if it.Type != "message" && it.Type != "fallback_message" {
			continue
		}
		if len(attempts) == 0 || attempts[len(attempts)-1].Model != it.Model {
			attempts = append(attempts, chat.UsageAttempt{Model: it.Model, Billed: true})
		}
		hop := &attempts[len(attempts)-1]
		hop.InputTokens += it.InputTokens
		hop.OutputTokens += it.OutputTokens
		hop.CachedInputTokens += it.CacheReadInputTokens
		hop.CacheWriteTokens += it.CacheCreationInputTokens
	}
	requestedRan := len(refusals) > 0 && refusals[0].from == requestedModel
	// On malformed chains only the prefix before any fallback target can use
	// the requested model's pricing, regardless of iteration type or aliases.
	for i := range attempts {
		if slices.ContainsFunc(refusals, func(r refusalEvidence) bool { return r.to == attempts[i].Model }) {
			break
		}
		attempts[i].Dispatched = requestedRan
	}
	if len(attempts) != len(refusals)+1 {
		return attempts
	}
	for i, refusal := range refusals {
		// The primary's from echoes the request alias; other marker IDs are canonical.
		primaryAlias := i == 0 && requestedRan
		if refusal.from == "" || refusal.to == "" || (!primaryAlias && refusal.from != attempts[i].Model) || refusal.to != attempts[i+1].Model {
			return attempts
		}
	}
	for i := range attempts {
		attempts[i].Dispatched = requestedRan && i == 0
	}

	for i := range attempts {
		var category respjson.Field
		if i < len(refusals) {
			category = refusals[i].category
		} else if stopReason == "refusal" {
			category = terminal
		}
		if attempts[i].OutputTokens == 0 && unbilledRefusalCategory(category) {
			attempts[i].Billed = false
		}
	}
	return attempts
}

func unbilledRefusalCategory(field respjson.Field) bool {
	if field.Raw() == "null" {
		return true
	}
	var category string
	if !field.Valid() || json.Unmarshal([]byte(field.Raw()), &category) != nil || category == "" {
		return false
	}
	return !billedBeforeOutput[category]
}
