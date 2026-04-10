package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/rag/prompts"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// createBetaStream creates a streaming chat completion using the Beta Messages API
// This is used when extended thinking is enabled via thinking_budget
func (c *Client) createBetaStream(
	ctx context.Context,
	client anthropic.Client,
	messages []chat.Message,
	requestTools []tools.Tool,
	maxTokens int64,
) (chat.MessageStream, error) {
	maxTokens, err := c.adjustMaxTokensForThinking(maxTokens)
	if err != nil {
		return nil, err
	}

	allTools, err := convertBetaTools(requestTools)
	if err != nil {
		slog.Error("Failed to convert tools for Anthropic Beta request", "error", err)
		return nil, err
	}

	converted, err := c.convertBetaMessages(ctx, messages)
	if err != nil {
		slog.Error("Failed to convert messages for Anthropic Beta request", "error", err)
		return nil, err
	}
	if err := validateAnthropicSequencingBeta(converted); err != nil {
		slog.Warn("Invalid message sequencing for Anthropic Beta API detected, attempting self-repair", "error", err)
		converted = repairAnthropicSequencingBeta(converted)
		if err2 := validateAnthropicSequencingBeta(converted); err2 != nil {
			slog.Error("Failed to self-repair Anthropic Beta sequencing", "error", err2)
			return nil, err
		}
	}
	if len(converted) == 0 {
		return nil, errors.New("no messages to send after conversion: all messages were filtered out")
	}

	sys := extractBetaSystemBlocks(messages)

	// Check if messages contain file attachments to include the files-api beta header
	needsFilesAPI := hasFileAttachments(messages)

	betas := []anthropic.AnthropicBeta{
		anthropic.AnthropicBetaInterleavedThinking2025_05_14,
		"fine-grained-tool-streaming-2025-05-14",
	}
	if needsFilesAPI {
		betas = append(betas, filesAPIBeta)
		slog.Debug("Anthropic Beta API: Including files-api beta header for file attachments")
	}

	params := anthropic.BetaMessageNewParams{
		Model:     c.ModelConfig.Model,
		MaxTokens: maxTokens,
		System:    sys,
		Messages:  converted,
		Tools:     allTools,
		Betas:     betas,
	}

	// Apply structured output configuration
	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.Debug("Anthropic Beta API using structured output", "name", structuredOutput.Name)

		// Add structured outputs beta header
		params.Betas = append(params.Betas, "structured-outputs-2025-11-13")

		// Configure output format using the SDK helper via OutputConfig
		params.OutputConfig = anthropic.BetaOutputConfigParam{
			Format: anthropic.BetaJSONSchemaOutputFormat(structuredOutput.Schema),
		}
	}

	// Configure thinking if a thinking budget is set in the model config.
	// The beta client is also used for structured output and file attachments,
	// which don't require thinking.
	if budget := c.ModelConfig.ThinkingBudget; budget != nil {
		if effort, ok := anthropicThinkingEffort(budget); ok {
			adaptive := anthropic.BetaThinkingConfigAdaptiveParam{}
			params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &adaptive}
			params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(effort)
			slog.Debug("Anthropic Beta API using adaptive thinking", "effort", effort)
		} else if tokens, ok := validThinkingTokens(int64(budget.Tokens), maxTokens); ok {
			params.Thinking = anthropic.BetaThinkingConfigParamOfEnabled(tokens)
			slog.Debug("Anthropic Beta API using thinking_budget", "budget_tokens", tokens)
		}
	}

	if len(requestTools) > 0 {
		slog.Debug("Anthropic Beta API: Adding tools to request", "tool_count", len(requestTools))

		// Apply tool_choice from agent config
		if toolChoice := c.ModelOptions.ToolChoice(); toolChoice != "" {
			switch toolChoice {
			case "required":
				params.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfAny: &anthropic.BetaToolChoiceAnyParam{}}
			case "none":
				params.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
			default: // "auto" or any other value
				params.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfAuto: &anthropic.BetaToolChoiceAutoParam{}}
			}
			slog.Debug("Anthropic Beta API request using tool_choice", "tool_choice", toolChoice)
		}
	}

	slog.Debug("Anthropic Beta API chat completion stream request",
		"model", params.Model,
		"max_tokens", maxTokens,
		"message_count", len(params.Messages))

	// Forward top_k from provider_opts (Anthropic natively supports it)
	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok {
		params.TopK = param.NewOpt(topK)
		slog.Debug("Anthropic Beta provider_opts: set top_k", "value", topK)
	}

	stream := client.Beta.Messages.NewStreaming(ctx, params)
	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
	ad := c.newBetaStreamAdapter(stream, trackUsage)

	// Set up single retry for context length errors
	ad.retryFn = func() *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion] {
		used, err := countAnthropicTokensBeta(ctx, client, c.ModelConfig.Model, converted, sys, allTools)
		if err != nil {
			slog.Warn("Failed to count tokens for retry, skipping", "error", err)
			return nil
		}
		newMaxTokens := clampMaxTokens(anthropicContextLimit(c.ModelConfig.Model), used, maxTokens)
		if newMaxTokens >= maxTokens {
			slog.Warn("Token count does not require clamping, not retrying")
			return nil
		}
		slog.Warn("Retrying with clamped max_tokens after context length error", "original", maxTokens, "clamped", newMaxTokens, "used", used)
		retryParams := params
		retryParams.MaxTokens = newMaxTokens
		return client.Beta.Messages.NewStreaming(ctx, retryParams)
	}

	slog.Debug("Anthropic Beta API chat completion stream created successfully", "model", c.ModelConfig.Model)
	return ad, nil
}

// validateAnthropicSequencingBeta performs the same validation as standard API but for Beta payloads
func validateAnthropicSequencingBeta(msgs []anthropic.BetaMessageParam) error {
	return validateSequencing(msgs)
}

// repairAnthropicSequencingBeta inserts a synthetic user message with tool_result blocks
// for any assistant tool_use blocks that don't have corresponding tool_result blocks
// in the immediate next user message.
func repairAnthropicSequencingBeta(msgs []anthropic.BetaMessageParam) []anthropic.BetaMessageParam {
	return repairSequencing(msgs, func(toolUseIDs map[string]struct{}) anthropic.BetaMessageParam {
		blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(toolUseIDs))
		for id := range toolUseIDs {
			slog.Debug("Creating synthetic tool_result", "tool_use_id", id)
			blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
				OfToolResult: &anthropic.BetaToolResultBlockParam{
					ToolUseID: id,
					Content: []anthropic.BetaToolResultBlockParamContentUnion{
						{OfText: &anthropic.BetaTextBlockParam{Text: "(tool execution failed)"}},
					},
				},
			})
		}
		return anthropic.BetaMessageParam{
			Role:    anthropic.BetaMessageParamRoleUser,
			Content: blocks,
		}
	})
}

// countAnthropicTokensBeta calls Anthropic's Count Tokens API for the provided Beta API payload
// and returns the number of input tokens.
func countAnthropicTokensBeta(
	ctx context.Context,
	client anthropic.Client,
	model anthropic.Model,
	messages []anthropic.BetaMessageParam,
	system []anthropic.BetaTextBlockParam,
	anthropicTools []anthropic.BetaToolUnionParam,
) (int64, error) {
	params := anthropic.BetaMessageCountTokensParams{
		Model:    model,
		Messages: messages,
	}
	if len(system) > 0 {
		params.System = anthropic.BetaMessageCountTokensParamsSystemUnion{
			OfBetaTextBlockArray: system,
		}
	}
	if len(anthropicTools) > 0 {
		// Convert BetaToolUnionParam to BetaMessageCountTokensParamsToolUnion
		toolParams := make([]anthropic.BetaMessageCountTokensParamsToolUnion, len(anthropicTools))
		for i, tool := range anthropicTools {
			if tool.OfTool != nil {
				toolParams[i] = anthropic.BetaMessageCountTokensParamsToolUnion{
					OfTool: tool.OfTool,
				}
			}
		}
		params.Tools = toolParams
	}

	result, err := client.Beta.Messages.CountTokens(ctx, params)
	if err != nil {
		return 0, err
	}
	return result.InputTokens, nil
}

// Rerank scores documents by relevance to the query using Anthropic's Beta Messages API
// with structured outputs. It returns relevance scores in the same order as input documents.
func (c *Client) Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error) {
	const logPrefix = "Anthropic reranking request"

	if len(documents) == 0 {
		slog.Debug(logPrefix, "model", c.ModelConfig.Model, "num_documents", 0)
		return []float64{}, nil
	}

	slog.Debug(logPrefix,
		"model", c.ModelConfig.Model,
		"query_length", len(query),
		"num_documents", len(documents),
		"has_criteria", criteria != "")

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create Anthropic client for reranking", "error", err)
		return nil, err
	}

	// Build user prompt with query and numbered documents (including metadata)
	userPrompt := prompts.BuildRerankDocumentsPrompt(query, documents)

	// Build system prompt with Anthropic-specific JSON schema instructions
	jsonFormatInstruction := `You MUST respond using the provided JSON schema, where "scores" is an array of numbers (one per document, in order).`
	systemPrompt := prompts.BuildRerankSystemPrompt(documents, criteria, c.ModelConfig.ProviderOpts, jsonFormatInstruction)

	// Construct minimal Beta messages payload.
	msgs := []anthropic.BetaMessageParam{
		{
			Role: anthropic.BetaMessageParamRoleUser,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: systemPrompt}},
				{OfText: &anthropic.BetaTextBlockParam{Text: userPrompt}},
			},
		},
	}

	// JSON schema for { "scores": [number, ...] }.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scores": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "number",
				},
			},
		},
		"required":             []string{"scores"},
		"additionalProperties": false,
	}

	// Default to 8192 if maxTokens is not set (0)
	// This is a safe default that works for all Anthropic models
	maxTokens := c.ModelOptions.MaxTokens()
	if maxTokens == 0 {
		maxTokens = 8192
	}
	params := anthropic.BetaMessageNewParams{
		Model:     c.ModelConfig.Model,
		MaxTokens: maxTokens,
		Messages:  msgs,
		// Enable structured outputs beta.
		Betas: []anthropic.AnthropicBeta{"structured-outputs-2025-11-13"},
		// Enforce schema for the output JSON via OutputConfig.
		OutputConfig: anthropic.BetaOutputConfigParam{
			Format: anthropic.BetaJSONSchemaOutputFormat(schema),
		},
	}

	// Apply user-configured sampling settings if specified.
	// For reranking, default temperature to 0 for deterministic scoring if not explicitly set.
	if c.ModelConfig.Temperature != nil {
		params.Temperature = param.NewOpt(*c.ModelConfig.Temperature)
	} else {
		params.Temperature = param.NewOpt(0.0)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = param.NewOpt(*c.ModelConfig.TopP)
	}

	// Forward top_k from provider_opts (Anthropic natively supports it)
	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok {
		params.TopK = param.NewOpt(topK)
		slog.Debug("Anthropic Beta provider_opts: set top_k", "value", topK)
	}

	// Use streaming API to avoid timeout errors for operations that may take longer than 10 minutes
	stream := client.Beta.Messages.NewStreaming(ctx, params)

	// Accumulate the full response from the stream
	resp, err := accumulateBetaStreamResponse(stream)
	if err != nil {
		slog.Error("Anthropic rerank streaming request failed", "error", err)
		return nil, fmt.Errorf("anthropic rerank request failed: %w", err)
	}

	rawJSON, err := extractAnthropicStructuredOutputJSON(resp)
	if err != nil {
		slog.Error("Failed to extract Anthropic structured output JSON", "error", err)
		return nil, err
	}

	scores, err := parseRerankScoresAnthropic(rawJSON, len(documents))
	if err != nil {
		slog.Error("Failed to parse Anthropic rerank scores", "error", err)
		return nil, err
	}

	slog.Debug("Anthropic reranking complete",
		"model", c.ModelConfig.Model,
		"num_scores", len(scores))

	return scores, nil
}

// extractAnthropicStructuredOutputJSON extracts the structured JSON string
// from a BetaMessage when using JSON outputs with output_format=json_schema.
// Per Anthropic docs, the JSON is returned as text content in response.content[0].text
// for models like claude-sonnet-4.5 when the structured-outputs beta is enabled.
func extractAnthropicStructuredOutputJSON(msg *anthropic.BetaMessage) (string, error) {
	if msg == nil {
		return "", errors.New("anthropic BetaMessage is nil")
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Anthropic BetaMessage: %w", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("failed to unmarshal Anthropic BetaMessage: %w", err)
	}

	content, ok := m["content"].([]any)
	if !ok {
		return "", errors.New("anthropic BetaMessage has no content")
	}

	for _, item := range content {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Look for the primary JSON text block
		if t, _ := part["type"].(string); t == "text" {
			if txt, ok := part["text"].(string); ok && strings.TrimSpace(txt) != "" {
				return txt, nil
			}
		}
	}

	return "", errors.New("no structured JSON text found in Anthropic BetaMessage content")
}

// parseRerankScoresAnthropic parses a JSON payload of the form {"scores":[...]} and validates length.
// This helper is local to the Anthropic provider to avoid cyclic dependencies.
func parseRerankScoresAnthropic(raw string, expected int) ([]float64, error) {
	type rerankResponse struct {
		Scores []float64 `json:"scores"`
	}

	raw = strings.TrimSpace(raw)

	tryParse := func(s string) ([]float64, error) {
		var rr rerankResponse
		if err := json.Unmarshal([]byte(s), &rr); err != nil {
			return nil, err
		}
		if len(rr.Scores) != expected {
			return nil, fmt.Errorf("expected %d scores, got %d", expected, len(rr.Scores))
		}
		return rr.Scores, nil
	}

	// First attempt: parse whole string as JSON.
	if scores, err := tryParse(raw); err == nil {
		return scores, nil
	}

	// Fallback: extract the first {...} block and try again, in case the model added prose.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		if scores, err := tryParse(raw[start : end+1]); err == nil {
			return scores, nil
		}
	}

	return nil, fmt.Errorf("invalid rerank JSON: %s", raw)
}

// accumulateBetaStreamResponse consumes a Beta streaming response and returns the final BetaMessage.
// This is needed for operations like reranking that require the complete response but must use
// streaming to avoid timeout errors.
func accumulateBetaStreamResponse(stream *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion]) (*anthropic.BetaMessage, error) {
	var messageID string
	var model string
	var role string
	var messageType string
	var textContent strings.Builder
	var stopReason string
	var stopSequence string
	var inputTokens int64
	var outputTokens int64
	var cacheCreationTokens int64
	var cacheReadTokens int64

	for stream.Next() {
		event := stream.Current()

		// Initialize the message metadata from the first event
		if messageID == "" {
			messageID = event.Message.ID
			model = event.Message.Model
			role = string(event.Message.Role)
			messageType = string(event.Message.Type)
		}

		// Handle different event types
		switch eventVariant := event.AsAny().(type) {
		case anthropic.BetaRawContentBlockDeltaEvent:
			if deltaVariant, ok := eventVariant.Delta.AsAny().(anthropic.BetaTextDelta); ok {
				textContent.WriteString(deltaVariant.Text)
			}
		case anthropic.BetaRawMessageDeltaEvent:
			stopReason = string(eventVariant.Delta.StopReason)
			stopSequence = eventVariant.Delta.StopSequence
			inputTokens = eventVariant.Usage.InputTokens
			outputTokens = eventVariant.Usage.OutputTokens
			cacheCreationTokens = eventVariant.Usage.CacheCreationInputTokens
			cacheReadTokens = eventVariant.Usage.CacheReadInputTokens
		}
	}

	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("stream error: %w", err)
	}

	// Build a BetaMessage using JSON marshaling to avoid union type issues
	// The extractAnthropicStructuredOutputJSON function will parse this correctly
	msgMap := map[string]any{
		"id":    messageID,
		"type":  messageType,
		"role":  role,
		"model": model,
		"content": []map[string]any{
			{
				"type": "text",
				"text": textContent.String(),
			},
		},
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
		"usage": map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": cacheCreationTokens,
			"cache_read_input_tokens":     cacheReadTokens,
		},
	}

	// Marshal and unmarshal to get a proper BetaMessage
	msgBytes, err := json.Marshal(msgMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal accumulated message: %w", err)
	}

	var msg anthropic.BetaMessage
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal accumulated message: %w", err)
	}

	return &msg, nil
}
