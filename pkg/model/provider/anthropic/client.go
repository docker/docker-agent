package anthropic

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

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
type Client struct {
	base.Config

	clientFn func(context.Context) (*APIClient, error)
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

	if err := validateThinkingOptions(cfg); err != nil {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", err)
		return nil, err
	}

	if err := validateCachePreservingUpdates(cfg); err != nil {
		return nil, err
	}
	if err := validateSpeed(cfg); err != nil {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", err)
		return nil, err
	}
	globalOptions := options.Apply(opts...)
	if err := validateFallbackHost(cfg, globalOptions.Gateway()); err != nil {
		slog.ErrorContext(ctx, "Anthropic client creation failed", "error", err)
		return nil, err
	}

	anthropicClient := &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
	}

	if gateway := globalOptions.Gateway(); gateway == "" {
		apiKey, fedAuth, err := buildDirectAuth(ctx, cfg, env)
		if err != nil {
			slog.ErrorContext(ctx, "Anthropic client creation failed", "error", err)
			return nil, err
		}
		httpClient := httpclient.NewHTTPClient(ctx)
		globalOptions.WrapTransport(ctx, httpClient)
		base.WrapOpenCodeSession(cfg, httpClient)

		baseURL := cmp.Or(cfg.BaseURL, os.Getenv("ANTHROPIC_BASE_URL"))
		anthropicClient.BaseURL = baseURL

		effectiveBase := cmp.Or(baseURL, defaultAnthropicBaseURL)
		var mw RequestMiddleware
		if fedAuth != nil {
			mw = func(req *http.Request, body []byte) (*http.Request, []byte, error) {
				if err := fedAuth.Authenticate(req.Context(), req, effectiveBase, httpClient); err != nil {
					return nil, nil, err
				}
				return req, body, nil
			}
		}

		client := NewAPIClient(APIClientConfig{
			BaseURL:    baseURL,
			APIKey:     apiKey,
			HTTPClient: httpClient,
			Middleware: mw,
		})
		anthropicClient.clientFn = func(context.Context) (*APIClient, error) {
			return client, nil
		}
	} else {
		if cfg.Auth != nil {
			return nil, errors.New("anthropic: auth and Docker AI Gateway are mutually exclusive")
		}
		if err := base.VerifyDockerGatewayAuth(ctx, env, gateway); err != nil {
			slog.ErrorContext(ctx, "Anthropic client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, err
		}

		anthropicClient.clientFn = func(ctx context.Context) (*APIClient, error) {
			connection, err := base.NewGatewayClient(ctx, env, gateway, "https://api.anthropic.com/", "/", cfg, &globalOptions)
			if err != nil {
				return nil, err
			}

			return NewAPIClient(APIClientConfig{
				BaseURL:    connection.BaseURL,
				APIKey:     connection.AuthToken,
				AuthToken:  connection.AuthToken,
				HTTPClient: connection.HTTPClient,
			}), nil
		}
	}

	slog.DebugContext(ctx, "Anthropic client created successfully", "model", cfg.Model)
	return anthropicClient, nil
}

// NewClientFromFactory creates a Client backed by the APIClient returned by
// factory. Config validation runs before factory is invoked.
func NewClientFromFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, factory func(context.Context) (*APIClient, error), opts ...options.Opt) (*Client, error) {
	var withOptions func(context.Context, options.ModelOptions) (*APIClient, error)
	if factory != nil {
		withOptions = func(ctx context.Context, _ options.ModelOptions) (*APIClient, error) {
			return factory(ctx)
		}
	}
	return NewClientFromFactoryWithOptions(ctx, cfg, env, withOptions, opts...)
}

// NewClientFromFactoryWithOptions is NewClientFromFactory for factories that
// need the applied options (e.g. options.TokenSource).
func NewClientFromFactoryWithOptions(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, factory func(context.Context, options.ModelOptions) (*APIClient, error), opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}
	if env == nil {
		return nil, errors.New("environment provider is required")
	}
	if factory == nil {
		return nil, errors.New("client factory is required")
	}
	if err := validateThinkingOptions(cfg); err != nil {
		return nil, err
	}
	if err := validateCachePreservingUpdates(cfg); err != nil {
		return nil, err
	}
	if err := validateSpeed(cfg); err != nil {
		return nil, err
	}
	globalOptions := options.Apply(opts...)

	client, err := factory(ctx, globalOptions)
	if err != nil {
		return nil, err
	}

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: func(context.Context) (*APIClient, error) {
			return client, nil
		},
	}, nil
}

func buildDirectAuth(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) (string, *federation.Authenticator, error) {
	if cfg.Auth != nil {
		if cfg.Auth.Type != latest.AuthTypeWorkloadIdentityFederation {
			return "", nil, fmt.Errorf("anthropic: unsupported auth.type %q", cfg.Auth.Type)
		}
		if cfg.Auth.Federation == nil {
			return "", nil, errors.New("anthropic: workload_identity_federation block is required when auth.type is workload_identity_federation")
		}
		slog.DebugContext(ctx, "Anthropic Workload Identity Federation configured",
			"federation_rule_id", cfg.Auth.Federation.FederationRuleID)
		auth, err := federation.NewAuthenticator(cfg.Auth.Federation, env)
		if err != nil {
			return "", nil, fmt.Errorf("anthropic workload identity federation: %w", err)
		}
		return "", auth, nil
	}
	tokenKey := cmp.Or(cfg.TokenKey, "ANTHROPIC_API_KEY")
	apiKey, _ := env.Get(ctx, tokenKey)
	if apiKey == "" {
		return "", nil, fmt.Errorf("%s environment variable is required", tokenKey)
	}
	slog.DebugContext(ctx, "Anthropic API key found", "token_key", tokenKey)
	return apiKey, nil, nil
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

	if _, ok := modelinfo.LookupClaude5(c.ModelConfig.Model); ok && providerutil.FinalAssistantPrefill(messages) {
		return nil, fmt.Errorf("%s does not accept a final assistant message (prefill)", c.ModelConfig.Model)
	}

	maxTokens := c.ModelOptions.MaxTokens()
	if maxTokens == 0 {
		maxTokens = 8192
	}
	maxTokens, err := c.adjustMaxTokensForThinking(maxTokens)
	if err != nil {
		return nil, err
	}
	maxTokens = c.floorMaxTokensForNoThinking(maxTokens)

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create Anthropic client", "error", err)
		return nil, err
	}

	if c.interleavedThinkingEnabled() ||
		c.ModelOptions.StructuredOutput() != nil ||
		!c.ModelConfig.TaskBudget.IsZero() || c.thinkingBindingBehavior() != "" ||
		c.progressUpdatesEnabled() ||
		cacheDiagnosticsEnabled(c.ModelConfig.ProviderOpts) || cachePreservingUpdatesEnabled(c.ModelConfig.ProviderOpts) || c.hasNativeCompaction(messages) ||
		fallbacksEnabled(c.ModelConfig.ProviderOpts) ||
		fastModeEnabled(c.ModelConfig.ProviderOpts) {
		return c.createBetaStream(ctx, client, messages, requestTools, maxTokens)
	}

	requestTools = c.toolsWithSupportedDeferral(ctx, requestTools, messages)
	allTools, err := convertTools(requestTools)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to convert tools for Anthropic request", "error", err)
		return nil, err
	}

	if sel, enabled := strictToolsOpt(c.ModelConfig.ProviderOpts); enabled {
		restoreStrictToolSchemas(allTools, requestTools)
		if err := sel.apply(allTools); err != nil {
			return nil, err
		}
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

	params := MessageRequest{
		Model:     c.ModelConfig.Model,
		MaxTokens: maxTokens,
		System:    sys,
		Messages:  converted,
		Tools:     allTools,
		Betas:     []AnthropicBeta{"fine-grained-tool-streaming-2025-05-14"},
	}

	thinkingEnabled := c.applyThinkingConfig(&params, maxTokens)

	if !thinkingEnabled && !rejectsSampling(c.ModelConfig.Model) {
		if c.ModelConfig.Temperature != nil {
			params.Temperature = new(*c.ModelConfig.Temperature)
		}
		if c.ModelConfig.TopP != nil {
			params.TopP = new(*c.ModelConfig.TopP)
		}
	} else if c.ModelConfig.Temperature != nil || c.ModelConfig.TopP != nil {
		slog.DebugContext(ctx, "Anthropic extended thinking enabled, ignoring temperature/top_p settings")
	}

	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok && !rejectsSampling(c.ModelConfig.Model) {
		params.TopK = new(topK)
		slog.DebugContext(ctx, "Anthropic provider_opts: set top_k", "value", topK)
	}

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to Anthropic request", "tool_count", len(requestTools))
	}

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

	stream := client.NewMessageStream(ctx, params)
	trackUsage := c.TrackUsageEnabled()
	ad := c.newStreamAdapter(stream, trackUsage)

	ad.retryFn = func() *EventStream {
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
		return client.NewMessageStream(ctx, retryParams)
	}

	slog.DebugContext(ctx, "Anthropic chat completion stream created successfully", "model", c.ModelConfig.Model)
	return ad, nil
}

func (c *Client) convertDoc(ctx context.Context, doc chat.Document) ([]ContentBlockParam, error) {
	return convertDocument(ctx, doc, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
}

func (c *Client) convertMessages(ctx context.Context, messages []chat.Message) ([]MessageParam, error) {
	return c.convertMessagesWithDeferred(ctx, messages, nil)
}

func (c *Client) convertMessagesWithDeferred(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) ([]MessageParam, error) {
	deferredByCallID := deferredToolNamesByCallID(requestTools)
	var anthropicMessages []MessageParam
	pendingAssistantToolUse := false
	loopStart := chat.ActiveToolLoopStart(messages)

	for i := 0; i < len(messages); i++ {
		msg := &messages[i]
		if msg.Role == chat.MessageRoleSystem {
			continue
		}
		if msg.Role == chat.MessageRoleUser {
			if len(msg.MultiContent) > 0 {
				contentBlocks, err := c.convertUserMultiContent(ctx, msg.MultiContent)
				if err != nil {
					return nil, err
				}
				if len(contentBlocks) > 0 {
					anthropicMessages = append(anthropicMessages, MessageParam{Role: "user", Content: contentBlocks})
				}
			} else {
				anthropicMessages = append(anthropicMessages, MessageParam{Role: "user", Content: []ContentBlockParam{newTextBlock(msg.Content)}})
			}
			continue
		}
		if msg.Role == chat.MessageRoleAssistant {
			if blocks, ok, err := replayContent(msg); err != nil {
				return nil, err
			} else if ok {
				anthropicMessages = append(anthropicMessages, MessageParam{Role: "assistant", Content: blocks})
				pendingAssistantToolUse = len(msg.ToolCalls) > 0
				continue
			}
			if blocks, ok := recoveredParams(msg); ok {
				anthropicMessages = append(anthropicMessages, MessageParam{Role: "assistant", Content: blocks})
				pendingAssistantToolUse = len(msg.ToolCalls) > 0
				continue
			}
			contentBlocks := make([]ContentBlockParam, 0)

			if include, err := c.flattenedThinking(i, loopStart, msg); err != nil {
				return nil, err
			} else if include {
				contentBlocks = append(contentBlocks, newThinkingBlock(msg.ThinkingSignature, msg.ReasoningContent))
			}

			if len(msg.ToolCalls) > 0 {
				if msg.Content != "" {
					contentBlocks = append(contentBlocks, newTextBlock(msg.Content))
				}
				for _, toolCall := range msg.ToolCalls {
					var inpts map[string]any
					if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &inpts); err != nil {
						inpts = map[string]any{}
					}
					contentBlocks = append(contentBlocks, ContentBlockParam{
						Type:  "tool_use",
						ID:    toolCall.ID,
						Input: inpts,
						Name:  toolCall.Function.Name,
					})
				}
				anthropicMessages = append(anthropicMessages, MessageParam{Role: "assistant", Content: contentBlocks})
				pendingAssistantToolUse = true
			} else {
				if msg.Content != "" {
					contentBlocks = append(contentBlocks, newTextBlock(msg.Content))
				}
				if len(contentBlocks) > 0 {
					anthropicMessages = append(anthropicMessages, MessageParam{Role: "assistant", Content: contentBlocks})
				}
				pendingAssistantToolUse = false
			}
			continue
		}
		if msg.Role == chat.MessageRoleTool {
			var blocks []ContentBlockParam
			j := i
			for j < len(messages) && messages[j].Role == chat.MessageRoleTool {
				if names := deferredByCallID[messages[j].ToolCallID]; len(names) > 0 {
					content := make([]ContentBlockParam, 0, len(names))
					for _, name := range names {
						content = append(content, ContentBlockParam{
							Type:     "tool_reference",
							ToolName: name,
						})
					}
					blocks = append(blocks, ContentBlockParam{
						Type:      "tool_result",
						ToolUseID: messages[j].ToolCallID,
						IsError:   new(messages[j].IsError),
						Content:   content,
					})
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
				if pendingAssistantToolUse {
					anthropicMessages = append(anthropicMessages, MessageParam{Role: "user", Content: blocks})
				}
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

func (c *Client) convertToolResultBlock(ctx context.Context, msg *chat.Message) (ContentBlockParam, error) {
	if !hasRichToolResultMultiContent(msg.MultiContent) {
		return newToolResultBlock(msg.ToolCallID, toolResultText(msg.Content), msg.IsError), nil
	}

	var content []ContentBlockParam
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			content = append(content, newTextBlock(part.Text))
		case chat.MessagePartTypeImageURL:
			content = append(content, toolResultImageContent(part)...)
		case chat.MessagePartTypeDocument:
			if part.Document == nil {
				continue
			}
			docBlocks, err := c.convertDoc(ctx, *part.Document)
			if err != nil {
				return ContentBlockParam{}, fmt.Errorf("failed to convert tool result document %q: %w", part.Document.Name, err)
			}
			content = append(content, docBlocks...)
		}
	}
	if len(content) == 0 {
		content = append(content, newTextBlock(toolResultText(msg.Content)))
	}

	return ContentBlockParam{
		Type:      "tool_result",
		ToolUseID: msg.ToolCallID,
		Content:   content,
		IsError:   new(msg.IsError),
	}, nil
}

func toolResultText(text string) string {
	if strings.TrimSpace(text) == "" {
		return "(no output)"
	}
	return text
}

func toolResultImageContent(part chat.MessagePart) []ContentBlockParam {
	if part.ImageURL == nil || !strings.HasPrefix(part.ImageURL.URL, "data:") {
		return nil
	}
	urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
	if len(urlParts) != 2 {
		return nil
	}
	mediaType := extractMediaType(urlParts[0])
	return []ContentBlockParam{{
		Type: "image",
		Source: &MediaSourceParam{
			Type:      "base64",
			Data:      urlParts[1],
			MediaType: mediaType,
		},
	}}
}

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

func (c *Client) convertUserMultiContent(ctx context.Context, parts []chat.MessagePart) ([]ContentBlockParam, error) {
	contentBlocks := make([]ContentBlockParam, 0, len(parts))

	for _, part := range parts {
		switch part.Type {
		case chat.MessagePartTypeText:
			contentBlocks = append(contentBlocks, newTextBlock(part.Text))

		case chat.MessagePartTypeImageURL:
			if part.ImageURL == nil {
				continue
			}
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
				if len(urlParts) == 2 {
					contentBlocks = append(contentBlocks, ContentBlockParam{
						Type: "image",
						Source: &MediaSourceParam{
							Type:      "base64",
							Data:      urlParts[1],
							MediaType: extractMediaType(urlParts[0]),
						},
					})
				}
			} else if strings.HasPrefix(part.ImageURL.URL, "http://") || strings.HasPrefix(part.ImageURL.URL, "https://") {
				contentBlocks = append(contentBlocks, ContentBlockParam{
					Type: "image",
					Source: &MediaSourceParam{
						Type: "url",
						URL:  part.ImageURL.URL,
					},
				})
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

func (c *Client) toolsWithSupportedDeferral(ctx context.Context, requestTools []tools.Tool, messages []chat.Message) []tools.Tool {
	if !c.supportsDeferredTools() {
		return withoutDeferral(requestTools, func(tools.Tool) bool { return true })
	}
	errorResults := errorToolResultIDs(messages)
	atErrorResult := func(tool tools.Tool) bool {
		return tool.Deferred && errorResults[tool.DeferredAtToolCallID]
	}
	if !slices.ContainsFunc(requestTools, atErrorResult) {
		return requestTools
	}
	slog.WarnContext(ctx, "Anthropic: deferred tools are anchored at an error tool result, loading them upfront")
	return withoutDeferral(requestTools, atErrorResult)
}

func withoutDeferral(requestTools []tools.Tool, undefer func(tools.Tool) bool) []tools.Tool {
	result := slices.Clip(slices.Clone(requestTools))
	if result == nil {
		result = []tools.Tool{}
	}
	for i := range result {
		if undefer(result[i]) {
			result[i].Deferred = false
			result[i].DeferredAtToolCallID = ""
		}
	}
	return result
}

func errorToolResultIDs(messages []chat.Message) map[string]bool {
	ids := make(map[string]bool)
	for i := range messages {
		if messages[i].Role == chat.MessageRoleTool && messages[i].IsError && messages[i].ToolCallID != "" {
			ids[messages[i].ToolCallID] = true
		}
	}
	return ids
}

func convertTools(tooles []tools.Tool) ([]ToolParam, error) {
	hasDeferredTools := containsDeferredTool(tooles)
	tooles, immediateCount := orderToolsForDeferredLoading(tooles)
	toolParams := make([]ToolParam, len(tooles))

	for i, tool := range tooles {
		inputSchema, err := ConvertParametersToSchema(tool.Parameters)
		if err != nil {
			return nil, err
		}

		toolParams[i] = ToolParam{
			Name:        tool.Name,
			Description: new(tool.Description),
			InputSchema: inputSchema,
		}
		if tool.Deferred {
			toolParams[i].DeferLoading = new(true)
		}
		if hasDeferredTools && i == immediateCount-1 {
			toolParams[i].CacheControl = newEphemeralCacheControl()
		}
	}
	return toolParams, nil
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
func ConvertParametersToSchema(params any) (ToolInputSchema, error) {
	var schema ToolInputSchema
	if err := tools.ConvertSchema(params, &schema); err != nil {
		return ToolInputSchema{}, err
	}

	m, err := tools.SchemaToMap(params)
	if err != nil {
		return ToolInputSchema{}, err
	}
	tools.OrderedSchemaProperties(m)
	schema.Properties = m["properties"]
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := m[key]; ok {
			if schema.ExtraFields == nil {
				schema.ExtraFields = make(map[string]any)
			}
			schema.ExtraFields[key] = defs
		}
	}
	return schema, nil
}

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

func (c *Client) contextLimit(ctx context.Context) int64 {
	if n := c.ModelConfig.DeclaredContextWindow(); n > 0 {
		return n
	}
	return modelinfo.ContextLimit(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), modelinfo.DefaultAnthropicContextLimit)
}

func clampMaxTokens(limit, used, configured int64) int64 {
	const safety = int64(1024)

	remaining := limit - used - safety
	remaining = max(remaining, 1)
	if configured > remaining {
		return remaining
	}
	return configured
}

func countAnthropicTokens(
	ctx context.Context,
	client *APIClient,
	model string,
	messages []MessageParam,
	system []TextBlockParam,
	anthropicTools []ToolParam,
	betas ...AnthropicBeta,
) (count int64, err error) {
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

	result, err := client.CountTokens(ctx, MessageCountTokensRequest{
		Model:    model,
		Messages: messages,
		System:   system,
		Tools:    anthropicTools,
		Betas:    betas,
	})
	if err != nil {
		return 0, err
	}
	return result.InputTokens, nil
}
