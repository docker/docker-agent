package anthropic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/tools"
)

// betaStreamAdapter adapts the Anthropic Beta stream to our interface
type betaStreamAdapter struct {
	retryableStream

	trackUsage     bool
	toolCall       bool
	stopReason     StopReason
	toolIDByBlock  map[int64]string
	message        Message
	rawLost        bool
	requestContext json.RawMessage
	provisional    atomic.Pointer[string]
	settled        atomic.Bool
	config         base.Config
	fallbackModels []string
	markerSeen     bool
	served         atomic.Pointer[string]
	refusals       []refusalEvidence
}

func (c *Client) newBetaStreamAdapter(stream *EventStream, trackUsage bool) *betaStreamAdapter {
	return &betaStreamAdapter{
		retryableStream: retryableStream{stream: stream},
		trackUsage:      trackUsage,
		toolIDByBlock:   map[int64]string{},
		config:          c.Config,
	}
}

func (a *betaStreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	ok, err := a.next()
	if !ok {
		return chat.MessageStreamResponse{}, wrapAnthropicError(err)
	}

	event := a.stream.Current()
	a.accumulate(event)

	var msgID, msgModel string
	if event.Message != nil {
		msgID = event.Message.ID
		msgModel = event.Message.Model
	}
	response := chat.MessageStreamResponse{
		ID:     msgID,
		Object: "chat.completion.chunk",
		Model:  msgModel,
		Choices: []chat.MessageStreamChoice{
			{
				Index: 0,
				Delta: chat.MessageDelta{
					Role: string(chat.MessageRoleAssistant),
				},
			},
		},
	}

	switch event.Type {
	case "message_start":
		if event.Message != nil {
			model := event.Message.Model
			if model != "" && model != a.config.ModelConfig.Model && slices.Contains(a.fallbackModels, model) {
				a.provisional.Store(&model)
			}
		}
	case "content_block_start":
		if block := event.ContentBlock; block != nil {
			switch block.Type {
			case "tool_use":
				a.toolIDByBlock[event.Index] = block.ID
				a.toolCall = true
				toolCall := tools.ToolCall{
					ID:   block.ID,
					Type: "function",
					Function: tools.FunctionCall{
						Name: block.Name,
					},
				}
				response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
			case "fallback":
				a.toolCall = false
				a.markerSeen = true
				toModel := block.To.Model
				a.served.Store(&toModel)
				category := block.Trigger.Category
				if block.Trigger.Type != "refusal" {
					category = RawField{}
				}
				a.refusals = append(a.refusals, refusalEvidence{from: block.From.Model, to: toModel, category: category})
				response.Choices[0].Delta.ServerFallback = &chat.ServerFallback{From: block.From.Model, To: toModel}
			case "thinking":
				if block.Thinking != "" {
					response.Choices[0].Delta.ReasoningContent = block.Thinking
					slog.Debug("Received thinking", "thinking", block.Thinking)
				}
				if block.Signature != "" {
					response.Choices[0].Delta.ThinkingSignature = block.Signature
				}
			}
		}
	case "content_block_delta":
		if delta := event.Delta; delta != nil {
			switch delta.Type {
			case "text_delta":
				response.Choices[0].Delta.Content = delta.Text
			case "thinking_delta":
				response.Choices[0].Delta.ReasoningContent = delta.Thinking
			case "input_json_delta":
				toolCall := tools.ToolCall{
					ID:   a.toolIDByBlock[event.Index],
					Type: "function",
					Function: tools.FunctionCall{
						Arguments: delta.PartialJSON,
					},
				}
				response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
			case "signature_delta":
				response.Choices[0].Delta.ThinkingSignature = delta.Signature
			default:
				return response, fmt.Errorf("unknown delta type: %s", delta.Type)
			}
		}
	case "message_delta":
		if event.Delta != nil {
			a.stopReason = event.Delta.StopReason
		}
		if !a.markerSeen {
			for _, it := range slices.Backward(a.message.Usage.Iterations) {
				if it.Type == "fallback_message" {
					m := it.Model
					a.served.Store(&m)
					break
				}
			}
		}
		a.settled.Store(true)
		if a.trackUsage {
			response.Usage = betaUsageFromMessage(a.message.Usage)
			terminal := a.message.StopDetails.Category
			if a.message.StopDetails.Type != "refusal" {
				terminal = RawField{}
			}
			response.UsageAttempts = serverFallbackAttempts(a.message.Usage.Iterations, a.refusals, a.config.ModelConfig.Model, a.stopReason, terminal)
		}
	case "message_stop":
		for _, dropped := range thinkingTransformations(a.message.InputTransformations) {
			slog.Warn("Anthropic dropped invalidated thinking", "block", dropped)
		}
		response.Choices[0].FinishReason = finishReason(a.stopReason, a.toolCall)
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

func (a *betaStreamAdapter) accumulate(event StreamEvent) {
	if a.rawLost && event.Type != "message_start" && event.Type != "message_delta" {
		return
	}
	if err := a.message.Accumulate(event); err != nil {
		a.rawLost = true
		slog.Debug("Anthropic Beta: raw response capture disabled", "error", err)
	}
}

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
	cfg.ModelConfig.DisplayModel = ""
	cfg.ModelConfig.Cost = nil
	return contracts.ServingModel{ID: cfg.ID(), Config: cfg}, true
}

func (a *betaStreamAdapter) Close() {
	a.close()
}

func betaUsageFromMessage(u Usage) *chat.Usage {
	return &chat.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
		ReasoningTokens:   u.OutputTokensDetails.ThinkingTokens,
		ServiceTier:       servedTier(u.Speed),
	}
}

var billedBeforeOutput = map[string]bool{"bio": true, "frontier_llm": true, "reasoning_extraction": true}

type refusalEvidence struct {
	from, to string
	category RawField
}

func serverFallbackAttempts(iterations []IterationUsage, refusals []refusalEvidence, requestedModel string, stopReason StopReason, terminal RawField) []chat.UsageAttempt {
	if !slices.ContainsFunc(iterations, func(it IterationUsage) bool { return it.Type == "fallback_message" }) {
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
		primaryAlias := i == 0 && requestedRan
		if refusal.from == "" || refusal.to == "" || (!primaryAlias && refusal.from != attempts[i].Model) || refusal.to != attempts[i+1].Model {
			return attempts
		}
	}
	for i := range attempts {
		attempts[i].Dispatched = requestedRan && i == 0
	}

	for i := range attempts {
		var category RawField
		if i < len(refusals) {
			category = refusals[i].category
		} else if stopReason == StopReasonRefusal {
			category = terminal
		}
		if attempts[i].OutputTokens == 0 && unbilledRefusalCategory(category) {
			attempts[i].Billed = false
		}
	}
	return attempts
}

func unbilledRefusalCategory(field RawField) bool {
	if field.Raw() == "null" {
		return true
	}
	var category string
	if !field.Valid() || json.Unmarshal([]byte(field.Raw()), &category) != nil || category == "" {
		return false
	}
	return !billedBeforeOutput[category]
}
