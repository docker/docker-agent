package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/federation"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client represents an Anthropic client wrapper implementing provider.Provider
// It holds the anthropic client and model config
type Client struct {
	base.Config

	clientFn func(context.Context) (anthropic.Client, error)
}

// NewClient creates a new Anthropic client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	if cfg.Provider != "anthropic" {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", "model type must be 'anthropic'", "actual_type", cfg.Provider)
		return nil, errors.New("model type must be 'anthropic'")
	}

	if env == nil {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", "environment provider is required")
		return nil, errors.New("environment provider is required")
	}

	if err := validateThinkingDisplay(cfg); err != nil {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", err)
		return nil, err
	}

	globalOptions := options.Apply(opts...)

	anthropicClient := &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
	}

	if gateway := globalOptions.Gateway(); gateway == "" {
		authOpts, err := buildDirectAuthOptions(ctx, cfg, env)
		if err != nil {
			slog.ErrorContext(ctx, "Anthropic client creation failed", "error", err)
			return nil, err
		}
		httpClient := httpclient.NewHTTPClient(ctx)
		globalOptions.WrapTransport(ctx, httpClient)
		requestOptions := append([]option.RequestOption{
			option.WithHTTPClient(httpClient),
		}, authOpts...)
		if cfg.BaseURL != "" {
			requestOptions = append(requestOptions, option.WithBaseURL(cfg.BaseURL))
		}
		client := anthropic.NewClient(requestOptions...)
		anthropicClient.clientFn = func(context.Context) (anthropic.Client, error) {
			return client, nil
		}
	} else {
		if cfg.Auth != nil {
			return nil, errors.New("anthropic: auth and Docker AI Gateway are mutually exclusive")
		}
		// When using a Gateway targeting a Docker domain, tokens are short-lived.
		// Only require and inject the Docker JWT if the gateway is a .docker.com URL.
		if err := base.VerifyDockerGatewayAuth(ctx, env, gateway); err != nil {
			slog.ErrorContext(ctx, "Anthropic client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, err
		}

		// When using a Gateway, tokens are short-lived.
		anthropicClient.clientFn = func(ctx context.Context) (anthropic.Client, error) {
			// Query a fresh auth token each time the client is used.
			authToken, err := base.GatewayAuthToken(ctx, env, gateway)
			if err != nil {
				return anthropic.Client{}, err
			}

			url, err := url.Parse(gateway)
			if err != nil {
				return anthropic.Client{}, fmt.Errorf("invalid gateway URL: %w", err)
			}
			baseURL := fmt.Sprintf("%s://%s%s/", url.Scheme, url.Host, url.Path)

			// Configure a custom HTTP client to inject headers and query params used by the Gateway.
			httpOptions := base.GatewayHTTPOptions(url, "https://api.anthropic.com/", cfg, &globalOptions)
			httpOptions = append(httpOptions, base.GatewayAuthRetry(env, gateway)...)

			gatewayHTTPClient := httpclient.NewHTTPClient(ctx, httpOptions...)
			globalOptions.WrapTransport(ctx, gatewayHTTPClient)
			clientOptions := []option.RequestOption{
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(gatewayHTTPClient),
			}
			if authToken != "" {
				clientOptions = append(clientOptions, option.WithAuthToken(authToken), option.WithAPIKey(authToken))
			}
			client := anthropic.NewClient(clientOptions...)

			return client, nil
		}
	}

	slog.DebugContext(ctx, "Anthropic client created successfully", "model", cfg.Model)
	return anthropicClient, nil
}

// NewClientFromFactory creates a Client backed by the SDK client returned by
// factory. Config validation runs before factory is invoked, so configuration
// errors surface without requiring provider credentials. The factory is
// called once during construction and the returned client is cached for the
// lifetime of the Client. It is the wiring point for subpackages that target
// alternative endpoints, such as
// [github.com/docker/docker-agent/pkg/model/provider/anthropic/vertex].
func NewClientFromFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, factory func(context.Context) (anthropic.Client, error), opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}
	if env == nil {
		return nil, errors.New("environment provider is required")
	}
	if factory == nil {
		return nil, errors.New("client factory is required")
	}
	if err := validateThinkingDisplay(cfg); err != nil {
		return nil, err
	}

	// Apply options before invoking the factory so option side effects keep
	// their pre-refactor ordering relative to credential discovery.
	globalOptions := options.Apply(opts...)

	client, err := factory(ctx)
	if err != nil {
		return nil, err
	}

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: func(context.Context) (anthropic.Client, error) {
			return client, nil
		},
	}, nil
}

// buildDirectAuthOptions returns the SDK request options that authenticate
// a direct (non-gateway) Anthropic client. It picks between Workload
// Identity Federation and the legacy ANTHROPIC_API_KEY path based on cfg.
func buildDirectAuthOptions(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) ([]option.RequestOption, error) {
	if cfg.Auth != nil {
		if cfg.Auth.Type != latest.AuthTypeWorkloadIdentityFederation {
			return nil, fmt.Errorf("anthropic: unsupported auth.type %q", cfg.Auth.Type)
		}
		// YAML-loaded configs are validated, but a programmatic caller may
		// pass Auth.Federation == nil; reject explicitly rather than panic.
		if cfg.Auth.Federation == nil {
			return nil, errors.New("anthropic: workload_identity_federation block is required when auth.type is workload_identity_federation")
		}
		slog.DebugContext(ctx, "Anthropic Workload Identity Federation configured",
			"federation_rule_id", cfg.Auth.Federation.FederationRuleID)
		opts, err := federation.RequestOptions(cfg.Auth.Federation, env)
		if err != nil {
			return nil, fmt.Errorf("anthropic workload identity federation: %w", err)
		}
		return opts, nil
	}
	apiKey, _ := env.Get(ctx, "ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY environment variable is required")
	}
	slog.DebugContext(ctx, "Anthropic API key found")
	return []option.RequestOption{option.WithAPIKey(apiKey)}, nil
}

// CreateChatCompletionStream creates a streaming chat completion request
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	slog.DebugContext(ctx, "Creating Anthropic chat completion stream",
		"model", c.ModelConfig.Model,
		"message_count", len(messages),
		"tool_count", len(requestTools))

	// Default to 8192 if maxTokens is not set (0)
	// This is a safe default that works for all Anthropic models
	maxTokens := c.ModelOptions.MaxTokens()
	if maxTokens == 0 {
		maxTokens = 8192
	}
	maxTokens, err := c.adjustMaxTokensForThinking(maxTokens)
	if err != nil {
		return nil, err
	}
	// Leave headroom for visible output when thinking is disabled with a tiny
	// cap (e.g. title generation): adaptive-thinking models still reason and
	// would otherwise consume the whole budget, returning empty text.
	maxTokens = c.floorMaxTokensForNoThinking(maxTokens)

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create Anthropic client", "error", err)
		return nil, err
	}

	// Route to the Beta Messages API when any of the following are set:
	//  - interleaved thinking
	//  - structured output (requires beta header)
	//  - task_budget (requires the task-budgets beta header)
	if c.interleavedThinkingEnabled() ||
		c.ModelOptions.StructuredOutput() != nil ||
		!c.ModelConfig.TaskBudget.IsZero() {
		return c.createBetaStream(ctx, client, messages, requestTools, maxTokens)
	}

	requestTools = c.toolsWithSupportedDeferral(requestTools)
	allTools, err := convertTools(requestTools)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to convert tools for Anthropic request", "error", err)
		return nil, err
	}

	converted, err := c.convertMessagesWithDeferred(ctx, messages, requestTools)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to convert messages for Anthropic request", "error", err)
		return nil, err
	}
	if len(converted) == 0 {
		return nil, errors.New("no messages to send after conversion: all messages were filtered out")
	}
	sys := extractSystemBlocks(messages)

	params := anthropic.MessageNewParams{
		Model:     c.ModelConfig.Model,
		MaxTokens: maxTokens,
		System:    sys,
		Messages:  converted,
		Tools:     allTools,
	}

	// Apply thinking budget first, as it affects whether we can set temperature
	thinkingEnabled := c.applyThinkingConfig(&params, maxTokens)

	// Temperature and TopP cannot be set when extended thinking is enabled
	// (Anthropic requires temperature=1.0 which is the default when thinking is on)
	if !thinkingEnabled {
		if c.ModelConfig.Temperature != nil {
			params.Temperature = param.NewOpt(*c.ModelConfig.Temperature)
		}
		if c.ModelConfig.TopP != nil {
			params.TopP = param.NewOpt(*c.ModelConfig.TopP)
		}
	} else if c.ModelConfig.Temperature != nil || c.ModelConfig.TopP != nil {
		slog.DebugContext(ctx, "Anthropic extended thinking enabled, ignoring temperature/top_p settings")
	}

	// Forward top_k from provider_opts (Anthropic natively supports it)
	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok {
		params.TopK = param.NewOpt(topK)
		slog.DebugContext(ctx, "Anthropic provider_opts: set top_k", "value", topK)
	}

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to Anthropic request", "tool_count", len(requestTools))
	}

	// Log the request details for debugging
	slog.DebugContext(ctx, "Anthropic chat completion stream request",
		"model", params.Model,
		"max_tokens", maxTokens,
		"message_count", len(params.Messages))

	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		b, err := json.Marshal(params)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to marshal Anthropic request", "error", err)
		}
		slog.DebugContext(ctx, "Request", "request", string(b))
	}

	// Add fine-grained tool streaming beta header; the standard Messages API
	// composes the anthropic-beta header manually (no params.Betas field).
	betas := "fine-grained-tool-streaming-2025-05-14"
	var requestOpts []option.RequestOption
	if fallbacks := fallbackModels(c.ModelConfig.ProviderOpts); len(fallbacks) > 0 {
		betas += "," + serverSideFallbackBeta
		requestOpts = append(requestOpts, fallbacksBody(fallbacks))
	}
	requestOpts = append(requestOpts, option.WithHeader("anthropic-beta", betas))

	stream := client.Messages.NewStreaming(ctx, params, requestOpts...)
	trackUsage := c.TrackUsageEnabled()
	ad := c.newStreamAdapter(stream, trackUsage)

	// Set up single retry for context length errors
	ad.retryFn = func() *ssestream.Stream[anthropic.MessageStreamEventUnion] {
		used, err := countAnthropicTokens(ctx, client, c.ModelConfig.Model, converted, sys, allTools)
		if err != nil {
			slog.WarnContext(ctx, "Failed to count tokens for retry, skipping", "error", err)
			return nil
		}
		newMaxTokens := clampMaxTokens(c.contextLimit(ctx), used, maxTokens)
		if newMaxTokens >= maxTokens {
			slog.WarnContext(ctx, "Token count does not require clamping, not retrying")
			return nil
		}
		slog.WarnContext(ctx, "Retrying with clamped max_tokens after context length error", "original max_tokens", maxTokens, "clamped max_tokens", newMaxTokens, "used tokens", used)
		retryParams := params
		retryParams.MaxTokens = newMaxTokens
		return client.Messages.NewStreaming(ctx, retryParams, requestOpts...)
	}

	slog.DebugContext(ctx, "Anthropic chat completion stream created successfully", "model", c.ModelConfig.Model)
	return ad, nil
}

// convertDoc converts a document attachment using the client's model ID
// and the store initialized at construction time.
func (c *Client) convertDoc(ctx context.Context, doc chat.Document) ([]anthropic.ContentBlockParamUnion, error) {
	return convertDocument(ctx, doc, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
}

func (c *Client) convertMessages(ctx context.Context, messages []chat.Message) ([]anthropic.MessageParam, error) {
	return c.convertMessagesWithDeferred(ctx, messages, nil)
}

func (c *Client) convertMessagesWithDeferred(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) ([]anthropic.MessageParam, error) {
	deferredByCallID := deferredToolNamesByCallID(requestTools)
	var anthropicMessages []anthropic.MessageParam
	// Track whether the last appended assistant message included tool_use blocks
	// so we can ensure the immediate next message is the grouped tool_result user message.
	pendingAssistantToolUse := false

	for i := 0; i < len(messages); i++ {
		msg := &messages[i]
		if msg.Role == chat.MessageRoleSystem {
			// System messages are handled via the top-level params.System
			continue
		}
		if msg.Role == chat.MessageRoleUser {
			// Handle MultiContent for user messages (including images and files)
			if len(msg.MultiContent) > 0 {
				contentBlocks, err := c.convertUserMultiContent(ctx, msg.MultiContent)
				if err != nil {
					return nil, err
				}
				if len(contentBlocks) > 0 {
					anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(contentBlocks...))
				}
			} else {
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
			}
			continue
		}
		if msg.Role == chat.MessageRoleAssistant {
			contentBlocks := make([]anthropic.ContentBlockParamUnion, 0)

			// Include thinking blocks when present to preserve extended thinking context
			if msg.ReasoningContent != "" && msg.ThinkingSignature != "" {
				contentBlocks = append(contentBlocks, anthropic.NewThinkingBlock(msg.ThinkingSignature, msg.ReasoningContent))
			} else if msg.ThinkingSignature != "" {
				contentBlocks = append(contentBlocks, anthropic.NewRedactedThinkingBlock(msg.ThinkingSignature))
			}

			if len(msg.ToolCalls) > 0 {
				blockLen := len(msg.ToolCalls)
				offset := 0
				if msg.Content != "" {
					blockLen++
				}
				toolUseBlocks := make([]anthropic.ContentBlockParamUnion, blockLen)
				// If there is prior thinking, append it first
				if len(contentBlocks) > 0 {
					toolUseBlocks = append(contentBlocks, toolUseBlocks...)
				}
				if msg.Content != "" {
					toolUseBlocks[len(contentBlocks)+offset] = anthropic.NewTextBlock(msg.Content)
					offset = 1
				}
				for j, toolCall := range msg.ToolCalls {
					var inpts map[string]any
					if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &inpts); err != nil {
						inpts = map[string]any{}
					}
					toolUseBlocks[len(contentBlocks)+j+offset] = anthropic.ContentBlockParamUnion{
						OfToolUse: &anthropic.ToolUseBlockParam{
							ID:    toolCall.ID,
							Input: inpts,
							Name:  toolCall.Function.Name,
						},
					}
				}
				anthropicMessages = append(anthropicMessages, anthropic.NewAssistantMessage(toolUseBlocks...))
				// Mark that we expect the very next message to be the grouped tool_result blocks.
				pendingAssistantToolUse = true
			} else {
				if msg.Content != "" {
					contentBlocks = append(contentBlocks, anthropic.NewTextBlock(msg.Content))
				}
				if len(contentBlocks) > 0 {
					anthropicMessages = append(anthropicMessages, anthropic.NewAssistantMessage(contentBlocks...))
				}
				// No tool_use in this assistant message
				pendingAssistantToolUse = false
			}
			continue
		}
		if msg.Role == chat.MessageRoleTool {
			// Group consecutive tool results into a single user message.
			//
			// This is to satisfy Anthropic's requirement that tool_use blocks are immediately followed
			// by a single user message containing all corresponding tool_result blocks.
			var blocks []anthropic.ContentBlockParamUnion
			j := i
			for j < len(messages) && messages[j].Role == chat.MessageRoleTool {
				if names := deferredByCallID[messages[j].ToolCallID]; len(names) > 0 {
					content := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(names))
					for _, name := range names {
						content = append(content, anthropic.ToolResultBlockParamContentUnion{
							OfToolReference: &anthropic.ToolReferenceBlockParam{ToolName: name},
						})
					}
					blocks = append(blocks, anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: messages[j].ToolCallID,
						IsError:   param.NewOpt(messages[j].IsError),
						Content:   content,
					}})
					j++
					continue
				}
				tr, err := c.convertToolResultBlock(ctx, &messages[j])
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, tr)
				j++
			}
			if len(blocks) > 0 {
				// Only include tool_result blocks if they immediately follow an assistant
				// message that contained tool_use. Otherwise, drop them to avoid invalid
				// sequencing errors.
				if pendingAssistantToolUse {
					anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(blocks...))
				}
				// Whether we used them or not, we've now handled the expected tool_result slot.
				pendingAssistantToolUse = false
			}
			i = j - 1
			continue
		}
	}

	applyMessageCacheControl(anthropicMessages, messageCacheBreakpoints(requestTools))

	return anthropicMessages, nil
}

func deferredToolNamesByCallID(requestTools []tools.Tool) map[string][]string {
	result := make(map[string][]string)
	for _, tool := range requestTools {
		if tool.Deferred && tool.DeferredAtToolCallID != "" {
			result[tool.DeferredAtToolCallID] = append(result[tool.DeferredAtToolCallID], tool.Name)
		}
	}
	return result
}

// convertToolResultBlock converts a tool message to an Anthropic tool_result block.
// Inline images and documents are included as native blocks within the tool_result.
func (c *Client) convertToolResultBlock(ctx context.Context, msg *chat.Message) (anthropic.ContentBlockParamUnion, error) {
	if !hasRichToolResultMultiContent(msg.MultiContent) {
		return anthropic.NewToolResultBlock(msg.ToolCallID, toolResultText(msg.Content), msg.IsError), nil
	}

	var content []anthropic.ToolResultBlockParamContentUnion
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: part.Text},
			})
		case chat.MessagePartTypeImageURL:
			content = append(content, toolResultImageContent(part)...)
		case chat.MessagePartTypeDocument:
			if part.Document == nil {
				continue
			}
			docBlocks, err := c.convertDoc(ctx, *part.Document)
			if err != nil {
				return anthropic.ContentBlockParamUnion{}, fmt.Errorf("failed to convert tool result document %q: %w", part.Document.Name, err)
			}
			content = append(content, stdBlocksToToolResultContent(docBlocks)...)
		}
	}
	if len(content) == 0 {
		content = append(content, anthropic.ToolResultBlockParamContentUnion{
			OfText: &anthropic.TextBlockParam{Text: toolResultText(msg.Content)},
		})
	}

	toolBlock := anthropic.ToolResultBlockParam{
		ToolUseID: msg.ToolCallID,
		Content:   content,
		IsError:   anthropic.Bool(msg.IsError),
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: &toolBlock}, nil
}

func toolResultText(text string) string {
	if strings.TrimSpace(text) == "" {
		return "(no output)"
	}
	return text
}

func toolResultImageContent(part chat.MessagePart) []anthropic.ToolResultBlockParamContentUnion {
	if part.ImageURL == nil || !strings.HasPrefix(part.ImageURL.URL, "data:") {
		return nil
	}
	urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
	if len(urlParts) != 2 {
		return nil
	}
	mediaType := extractMediaType(urlParts[0])
	return []anthropic.ToolResultBlockParamContentUnion{{
		OfImage: &anthropic.ImageBlockParam{
			Source: anthropic.ImageBlockParamSourceUnion{
				OfBase64: &anthropic.Base64ImageSourceParam{
					Data:      urlParts[1],
					MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
				},
			},
		},
	}}
}

func stdBlocksToToolResultContent(blocks []anthropic.ContentBlockParamUnion) []anthropic.ToolResultBlockParamContentUnion {
	out := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.OfText != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfText: block.OfText})
		case block.OfImage != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfImage: block.OfImage})
		case block.OfDocument != nil:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfDocument: block.OfDocument})
		}
	}
	return out
}

// hasRichToolResultMultiContent returns true if multi-content contains blocks
// that need Anthropic's array-shaped tool_result content.
func hasRichToolResultMultiContent(parts []chat.MessagePart) bool {
	for _, part := range parts {
		switch part.Type {
		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil {
				return true
			}
		case chat.MessagePartTypeDocument:
			if part.Document != nil {
				return true
			}
		}
	}
	return false
}

// extractMediaType extracts the media type from a data URL prefix (e.g. "data:image/png;base64").
func extractMediaType(prefix string) string {
	switch {
	case strings.Contains(prefix, "image/jpeg"):
		return "image/jpeg"
	case strings.Contains(prefix, "image/png"):
		return "image/png"
	case strings.Contains(prefix, "image/gif"):
		return "image/gif"
	case strings.Contains(prefix, "image/webp"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// convertUserMultiContent converts user message multi-content parts to Anthropic content blocks.
// It handles text, legacy image URLs, and document attachments.
func (c *Client) convertUserMultiContent(ctx context.Context, parts []chat.MessagePart) ([]anthropic.ContentBlockParamUnion, error) {
	contentBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(parts))

	for _, part := range parts {
		switch part.Type {
		case chat.MessagePartTypeText:
			contentBlocks = append(contentBlocks, anthropic.NewTextBlock(part.Text))

		case chat.MessagePartTypeImageURL:
			if part.ImageURL == nil {
				continue
			}
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
				if len(urlParts) == 2 {
					contentBlocks = append(contentBlocks, anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{
						Data:      urlParts[1],
						MediaType: anthropic.Base64ImageSourceMediaType(extractMediaType(urlParts[0])),
					}))
				}
			} else if strings.HasPrefix(part.ImageURL.URL, "http://") || strings.HasPrefix(part.ImageURL.URL, "https://") {
				contentBlocks = append(contentBlocks, anthropic.NewImageBlock(anthropic.URLImageSourceParam{
					URL: part.ImageURL.URL,
				}))
			}

		case chat.MessagePartTypeFile:
			if part.File != nil {
				slog.WarnContext(ctx, "anthropic: dropping legacy file attachment; use document attachments instead", "path", part.File.Path)
			}

		case chat.MessagePartTypeDocument:
			if part.Document != nil {
				docBlocks, err := c.convertDoc(ctx, *part.Document)
				if err != nil {
					return nil, fmt.Errorf("failed to convert document attachment %q: %w", part.Document.Name, err)
				}
				contentBlocks = append(contentBlocks, docBlocks...)
			}
		}
	}

	return contentBlocks, nil
}

func (c *Client) supportsDeferredTools() bool {
	if enabled, ok := providerutil.GetProviderOptBool(c.ModelConfig.ProviderOpts, "supports_deferred_tools"); ok {
		return enabled
	}
	return modelinfo.SupportsDeferredTools(c.ModelConfig.Provider, c.ModelConfig.Model)
}

func (c *Client) toolsWithSupportedDeferral(requestTools []tools.Tool) []tools.Tool {
	if c.supportsDeferredTools() {
		return requestTools
	}
	result := make([]tools.Tool, len(requestTools))
	copy(result, requestTools)
	for i := range result {
		result[i].Deferred = false
		result[i].DeferredAtToolCallID = ""
	}
	return result
}

func convertTools(tooles []tools.Tool) ([]anthropic.ToolUnionParam, error) {
	hasDeferredTools := containsDeferredTool(tooles)
	tooles, immediateCount := orderToolsForDeferredLoading(tooles)
	toolParams := make([]anthropic.ToolParam, len(tooles))

	for i, tool := range tooles {
		inputSchema, err := ConvertParametersToSchema(tool.Parameters)
		if err != nil {
			return nil, err
		}

		toolParams[i] = anthropic.ToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			InputSchema: inputSchema,
		}
		if tool.Deferred {
			toolParams[i].DeferLoading = param.NewOpt(true)
		}
		if hasDeferredTools && i == immediateCount-1 {
			toolParams[i].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}
	anthropicTools := make([]anthropic.ToolUnionParam, len(toolParams))
	for i := range toolParams {
		anthropicTools[i] = anthropic.ToolUnionParam{OfTool: &toolParams[i]}
	}

	return anthropicTools, nil
}

func containsDeferredTool(requestTools []tools.Tool) bool {
	for _, tool := range requestTools {
		if tool.Deferred {
			return true
		}
	}
	return false
}

func orderToolsForDeferredLoading(requestTools []tools.Tool) ([]tools.Tool, int) {
	ordered := make([]tools.Tool, 0, len(requestTools))
	for _, tool := range requestTools {
		if !tool.Deferred {
			ordered = append(ordered, tool)
		}
	}
	immediateCount := len(ordered)
	if immediateCount == 0 {
		ordered = append(ordered, requestTools...)
		for i := range ordered {
			ordered[i].Deferred = false
		}
		return ordered, len(ordered)
	}
	for _, tool := range requestTools {
		if tool.Deferred {
			ordered = append(ordered, tool)
		}
	}
	return ordered, immediateCount
}

// ConvertParametersToSchema converts parameters to Anthropic Schema format
func ConvertParametersToSchema(params any) (anthropic.ToolInputSchemaParam, error) {
	var schema anthropic.ToolInputSchemaParam
	if err := tools.ConvertSchema(params, &schema); err != nil {
		return anthropic.ToolInputSchemaParam{}, err
	}

	return schema, nil
}

// marshalToMap is a helper that converts any value to a map[string]any via JSON marshaling.
// This is used to inspect SDK union types without depending on their internal structure.
// It's shared by both standard and Beta API validation/repair code.
func marshalToMap(v any) (map[string]any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return m, true
}

// contentArray extracts the content array from a marshaled message map.
// Used by both standard and Beta API validation/repair code.
func contentArray(m map[string]any) []any {
	if a, ok := m["content"].([]any); ok {
		return a
	}
	return nil
}

// contextLimit returns the context window for this client's model, sourced
// from models.dev (including the embedded snapshot) when available, and
// falling back to the conservative 200k Claude floor only when no catalogue
// entry exists.
func (c *Client) contextLimit(ctx context.Context) int64 {
	return modelinfo.ContextLimit(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), modelinfo.DefaultAnthropicContextLimit)
}

// clampMaxTokens returns the effective max_tokens value after capping to the
// remaining context window (limit - used - safety), clamped to at least 1.
func clampMaxTokens(limit, used, configured int64) int64 {
	const safety = int64(1024)

	remaining := limit - used - safety
	remaining = max(remaining, 1)
	if configured > remaining {
		return remaining
	}
	return configured
}

// countAnthropicTokens calls Anthropic's Count Tokens API for the provided payload
// and returns the number of input tokens.
func countAnthropicTokens(
	ctx context.Context,
	client anthropic.Client,
	model anthropic.Model,
	messages []anthropic.MessageParam,
	system []anthropic.TextBlockParam,
	anthropicTools []anthropic.ToolUnionParam,
) (count int64, err error) {
	// Token counting is a blocking API call to Anthropic that fires
	// on the context-overflow retry path. Span it so the latency is
	// attributable when the retry stalls.
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/model/provider/anthropic").Start(
		ctx,
		"anthropic.tokens.count",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(genai.AttrProviderName, genai.ProviderAnthropic),
			attribute.String(genai.AttrRequestModel, model),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		if count > 0 {
			span.SetAttributes(attribute.Int64("cagent.anthropic.tokens.counted", count))
		}
		span.End()
	}()

	params := anthropic.MessageCountTokensParams{
		Model:    model,
		Messages: messages,
	}
	if len(system) > 0 {
		params.System = anthropic.MessageCountTokensParamsSystemUnion{
			OfTextBlockArray: system,
		}
	}
	if len(anthropicTools) > 0 {
		// Convert ToolUnionParam to MessageCountTokensToolUnionParam
		toolParams := make([]anthropic.MessageCountTokensToolUnionParam, len(anthropicTools))
		for i, tool := range anthropicTools {
			if tool.OfTool != nil {
				toolParams[i] = anthropic.MessageCountTokensToolUnionParam{
					OfTool: tool.OfTool,
				}
			}
		}
		params.Tools = toolParams
	}

	result, err := client.Messages.CountTokens(ctx, params)
	if err != nil {
		return 0, err
	}
	return result.InputTokens, nil
}
