package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/rag/prompts"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

func (c *Client) createBetaStream(
	ctx context.Context,
	client *APIClient,
	messages []chat.Message,
	requestTools []tools.Tool,
	maxTokens int64,
) (chat.MessageStream, error) {
	maxTokens, err := c.adjustMaxTokensForThinking(maxTokens)
	if err != nil {
		return nil, err
	}

	requestTools = c.toolsWithSupportedDeferral(ctx, requestTools, messages)
	allTools, err := convertTools(requestTools)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to convert tools for Anthropic Beta request", "error", err)
		return nil, err
	}

	if sel, enabled := strictToolsOpt(c.ModelConfig.ProviderOpts); enabled {
		restoreStrictToolSchemas(allTools, requestTools)
		var schema any
		if output := c.ModelOptions.StructuredOutput(); output != nil {
			schema = output.Schema
		}
		if err := sel.apply(allTools, schema); err != nil {
			return nil, err
		}
	}

	converted, err := c.convertBetaMessagesWithDeferred(ctx, messages, requestTools)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to convert messages for Anthropic Beta request", "error", err)
		return nil, err
	}
	if len(converted) == 0 {
		return nil, errors.New("no messages to send after conversion: all messages were filtered out")
	}

	sys, transient := c.betaSystemContext(messages)

	betas := []AnthropicBeta{"fine-grained-tool-streaming-2025-05-14"}
	if _, set := c.ModelConfig.ProviderOpts["interleaved_thinking"]; !set || c.interleavedThinkingEnabled() {
		betas = append([]AnthropicBeta{AnthropicBetaInterleavedThinking2025_05_14}, betas...)
	}

	params := MessageRequest{
		Model:     c.ModelConfig.Model,
		MaxTokens: maxTokens,
		System:    sys,
		Messages:  converted,
		Tools:     allTools,
		Betas:     betas,
		Beta:      true,
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.DebugContext(ctx, "Anthropic Beta API using structured output", "name", structuredOutput.Name)
		params.Betas = append(params.Betas, "structured-outputs-2025-11-13")
		if params.OutputConfig == nil {
			params.OutputConfig = &OutputConfigParam{}
		}
		params.OutputConfig.Format = &JSONSchemaOutputFormat{
			Type:   "json_schema",
			Schema: structuredOutput.Schema,
		}
	}

	c.applyBetaThinkingConfig(&params, maxTokens)
	c.applyThinkingBinding(&params)

	thinkingEnabled := params.Thinking != nil && (params.Thinking.Type == "enabled" || params.Thinking.Type == "adaptive")
	if !thinkingEnabled && !rejectsSampling(c.ModelConfig.Model) {
		if c.ModelConfig.Temperature != nil {
			params.Temperature = new(*c.ModelConfig.Temperature)
		}
		if c.ModelConfig.TopP != nil {
			params.TopP = new(*c.ModelConfig.TopP)
		}
	}

	configureTaskBudget(&params, c.ModelConfig.TaskBudget)
	applySpeed(&params, c.ModelConfig.ProviderOpts)

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Anthropic Beta API: Adding tools to request", "tool_count", len(requestTools))
	}

	slog.DebugContext(ctx, "Anthropic Beta API chat completion stream request",
		"model", params.Model,
		"max_tokens", maxTokens,
		"message_count", len(params.Messages))

	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok && !rejectsSampling(c.ModelConfig.Model) {
		params.TopK = new(topK)
		slog.DebugContext(ctx, "Anthropic Beta provider_opts: set top_k", "value", topK)
	}

	if spec, _ := parseFallbacks(c.ModelConfig.ProviderOpts); spec.enabled() {
		params.Betas = append(params.Betas, serverSideFallbackBeta)
		params.Fallbacks = spec.param()
	}

	configureCacheDiagnostics(&params, messages, c.ModelConfig.ProviderOpts)
	if c.hasNativeCompaction(messages) {
		params.Betas = append(params.Betas, nativeCompactionBeta)
	}
	var requestContext json.RawMessage
	if cachePreservingUpdatesEnabled(c.ModelConfig.ProviderOpts) {
		updates, err := applyConversationUpdates(ctx, c.ModelConfig.Model, &params, messages, transient)
		if err != nil {
			return nil, err
		}
		requestContext = updates.RequestContext
	}
	stream := client.NewMessageStream(ctx, params)
	trackUsage := c.TrackUsageEnabled()
	ad := c.newBetaStreamAdapter(stream, trackUsage)
	ad.requestContext = requestContext
	ad.fallbackModels = fallbackTargets(c.ModelConfig.ProviderOpts)
	for _, message := range params.Messages {
		if message.Role != "assistant" {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "fallback" && len(block.Raw) > 0 {
				var fb struct {
					To FallbackEndpoint `json:"to"`
				}
				if json.Unmarshal(block.Raw, &fb) == nil && fb.To.Model != "" {
					ad.fallbackModels = append(ad.fallbackModels, fb.To.Model)
				}
			}
		}
	}

	ad.retryFn = func() *EventStream {
		used, err := countAnthropicTokens(ctx, client, c.ModelConfig.Model, params.Messages, params.System, params.Tools, params.Betas...)
		if err != nil {
			slog.WarnContext(ctx, "Failed to count tokens for retry, skipping", "error", err)
			return nil
		}
		newMaxTokens := clampMaxTokens(c.contextLimit(ctx), used, maxTokens)
		if newMaxTokens >= maxTokens {
			slog.WarnContext(ctx, "Token count does not require clamping, not retrying")
			return nil
		}
		slog.WarnContext(ctx, "Retrying with clamped max_tokens after context length error", "original", maxTokens, "clamped", newMaxTokens, "used", used)
		retryParams := params
		retryParams.MaxTokens = newMaxTokens
		return client.NewMessageStream(ctx, retryParams)
	}

	slog.DebugContext(ctx, "Anthropic Beta API chat completion stream created successfully", "model", c.ModelConfig.Model)
	return ad, nil
}

// Rerank scores documents by relevance to the query using Anthropic's Beta Messages API
// with structured outputs. It returns relevance scores in the same order as input documents.
func (c *Client) Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error) {
	const logPrefix = "Anthropic reranking request"

	if len(documents) == 0 {
		slog.DebugContext(ctx, logPrefix, "model", c.ModelConfig.Model, "num_documents", 0)
		return []float64{}, nil
	}

	slog.DebugContext(ctx, logPrefix,
		"model", c.ModelConfig.Model,
		"query_length", len(query),
		"num_documents", len(documents),
		"has_criteria", criteria != "")

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create Anthropic client for reranking", "error", err)
		return nil, err
	}

	userPrompt := prompts.BuildRerankDocumentsPrompt(query, documents)
	jsonFormatInstruction := `You MUST respond using the provided JSON schema, where "scores" is an array of numbers (one per document, in order).`
	systemPrompt := prompts.BuildRerankSystemPrompt(documents, criteria, c.ModelConfig.ProviderOpts, jsonFormatInstruction)

	msgs := []MessageParam{
		{
			Role: "user",
			Content: []ContentBlockParam{
				newTextBlock(systemPrompt),
				newTextBlock(userPrompt),
			},
		},
	}

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

	maxTokens := c.ModelOptions.MaxTokens()
	if maxTokens == 0 {
		maxTokens = 8192
	}
	params := MessageRequest{
		Model:     c.ModelConfig.Model,
		MaxTokens: maxTokens,
		Messages:  msgs,
		Betas:     []AnthropicBeta{"structured-outputs-2025-11-13"},
		Beta:      true,
		OutputConfig: &OutputConfigParam{
			Format: &JSONSchemaOutputFormat{
				Type:   "json_schema",
				Schema: schema,
			},
		},
	}

	if !rejectsSampling(c.ModelConfig.Model) && c.ModelConfig.Temperature != nil {
		params.Temperature = new(*c.ModelConfig.Temperature)
	} else if !rejectsSampling(c.ModelConfig.Model) {
		params.Temperature = new(0.0)
	}
	if !rejectsSampling(c.ModelConfig.Model) && c.ModelConfig.TopP != nil {
		params.TopP = new(*c.ModelConfig.TopP)
	}

	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok && !rejectsSampling(c.ModelConfig.Model) {
		params.TopK = new(topK)
		slog.DebugContext(ctx, "Anthropic Beta provider_opts: set top_k", "value", topK)
	}
	applySpeed(&params, c.ModelConfig.ProviderOpts)

	stream := client.NewMessageStream(ctx, params)
	defer stream.Close()

	resp, err := accumulateStreamResponse(stream)
	if err != nil {
		slog.ErrorContext(ctx, "Anthropic rerank streaming request failed", "error", err)
		return nil, fmt.Errorf("anthropic rerank request failed: %w", err)
	}

	rawJSON, err := extractAnthropicStructuredOutputJSON(resp)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to extract Anthropic structured output JSON", "error", err)
		return nil, err
	}

	scores, err := providerutil.ParseRerankScores(rawJSON, len(documents))
	if err != nil {
		slog.ErrorContext(ctx, "Failed to parse Anthropic rerank scores", "error", err)
		return nil, err
	}

	slog.DebugContext(ctx, "Anthropic reranking complete",
		"model", c.ModelConfig.Model,
		"num_scores", len(scores))

	return scores, nil
}

func extractAnthropicStructuredOutputJSON(msg *Message) (string, error) {
	if msg == nil {
		return "", errors.New("anthropic Message is nil")
	}
	if len(msg.Content) == 0 {
		return "", errors.New("anthropic Message has no content")
	}
	for _, block := range msg.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return block.Text, nil
		}
	}
	return "", errors.New("no structured JSON text found in Anthropic Message content")
}

func accumulateStreamResponse(stream *EventStream) (*Message, error) {
	var msg Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("accumulate stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("stream error: %w", err)
	}
	return &msg, nil
}

const taskBudgetBeta AnthropicBeta = "task-budgets-2026-03-13"

func configureTaskBudget(params *MessageRequest, tb *latest.TaskBudget) {
	payload := tb.AsMap()
	if payload == nil {
		return
	}
	params.Betas = append(params.Betas, taskBudgetBeta)
	if params.OutputConfig == nil {
		params.OutputConfig = &OutputConfigParam{}
	}
	params.OutputConfig.TaskBudget = payload
	slog.Debug("Anthropic Beta API using task_budget", "type", payload["type"], "total", payload["total"])
}
