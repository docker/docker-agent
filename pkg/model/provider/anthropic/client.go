package anthropic

import (
	"cmp"
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

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/federation"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client represents an Anthropic client wrapper implementing provider.Provider
// It holds the anthropic client and model config
type Client struct {
	base.Config

	clientFn    func(context.Context) (anthropic.Client, error)
	fileManager *FileManager
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

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&globalOptions)
		}
	}

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
		requestOptions := append([]option.RequestOption{
			option.WithHTTPClient(httpclient.NewHTTPClient(ctx)),
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
		// Fail fast if Docker Desktop's auth token isn't available
		if token, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); token == "" {
			slog.ErrorContext(ctx, "Anthropic client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, errors.New("sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
		}

		// When using a Gateway, tokens are short-lived.
		anthropicClient.clientFn = func(ctx context.Context) (anthropic.Client, error) {
			// Query a fresh auth token each time the client is used
			authToken, _ := env.Get(ctx, environment.DockerDesktopTokenEnv)
			if authToken == "" {
				return anthropic.Client{}, errors.New(base.NoDesktopTokenErrorMessage)
			}

			url, err := url.Parse(gateway)
			if err != nil {
				return anthropic.Client{}, fmt.Errorf("invalid gateway URL: %w", err)
			}
			baseURL := fmt.Sprintf("%s://%s%s/", url.Scheme, url.Host, url.Path)

			// Configure a custom HTTP client to inject headers and query params used by the Gateway.
			httpOptions := []httpclient.Opt{
				httpclient.WithProxiedBaseURL(cmp.Or(cfg.BaseURL, "https://api.anthropic.com/")),
				httpclient.WithProvider(cfg.Provider),
				httpclient.WithModel(cfg.Model),
				httpclient.WithModelName(cfg.Name),
				httpclient.WithQuery(url.Query()),
			}
			if globalOptions.GeneratingTitle() {
				httpOptions = append(httpOptions, httpclient.WithHeader("X-Cagent-GeneratingTitle", "1"))
			}

			client := anthropic.NewClient(
				option.WithAuthToken(authToken),
				option.WithAPIKey(authToken),
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(httpclient.NewHTTPClient(ctx, httpOptions...)),
			)

			return client, nil
		}
	}

	slog.DebugContext(ctx, "Anthropic client created successfully", "model", cfg.Model)

	// Initialize FileManager for file uploads
	anthropicClient.fileManager = NewFileManager(anthropicClient.clientFn)

	return anthropicClient, nil
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

// hasFileAttachments checks if any messages contain file attachments.
// This is used to determine if we need to use the Beta API (Files API is Beta-only).
func hasFileAttachments(messages []chat.Message) bool {
	for i := range messages {
		for _, part := range messages[i].MultiContent {
			if part.Type == chat.MessagePartTypeFile && part.File != nil {
				return true
			}
		}
	}
	return false
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

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create Anthropic client", "error", err)
		return nil, err
	}

	// Route to the Beta Messages API when any of the following are set:
	//  - interleaved thinking
	//  - structured output (requires beta header)
	//  - file attachments (Files API is Beta-only)
	//  - task_budget (requires the task-budgets beta header)
	if c.interleavedThinkingEnabled() ||
		c.ModelOptions.StructuredOutput() != nil ||
		hasFileAttachments(messages) ||
		!c.ModelConfig.TaskBudget.IsZero() {
		return c.createBetaStream(ctx, client, messages, requestTools, maxTokens)
	}

	allTools, err := convertTools(requestTools)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to convert tools for Anthropic request", "error", err)
		return nil, err
	}

	converted, err := c.convertMessages(ctx, messages)
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

	// Add fine-grained tool streaming beta header
	betaHeader := option.WithHeader("anthropic-beta", "fine-grained-tool-streaming-2025-05-14")

	stream := client.Messages.NewStreaming(ctx, params, betaHeader)
	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
	ad := c.newStreamAdapter(stream, trackUsage)

	// Set up single retry for context length errors
	ad.retryFn = func() *ssestream.Stream[anthropic.MessageStreamEventUnion] {
		used, err := countAnthropicTokens(ctx, client, c.ModelConfig.Model, converted, sys, allTools)
		if err != nil {
			slog.WarnContext(ctx, "Failed to count tokens for retry, skipping", "error", err)
			return nil
		}
		newMaxTokens := clampMaxTokens(anthropicContextLimit(c.ModelConfig.Model), used, maxTokens)
		if newMaxTokens >= maxTokens {
			slog.WarnContext(ctx, "Token count does not require clamping, not retrying")
			return nil
		}
		slog.WarnContext(ctx, "Retrying with clamped max_tokens after context length error", "original max_tokens", maxTokens, "clamped max_tokens", newMaxTokens, "used tokens", used)
		retryParams := params
		retryParams.MaxTokens = newMaxTokens
		return client.Messages.NewStreaming(ctx, retryParams, betaHeader)
	}

	slog.DebugContext(ctx, "Anthropic chat completion stream created successfully", "model", c.ModelConfig.Model)
	return ad, nil
}

// convertDoc converts a document attachment using the client's model ID
// and the store initialized at construction time.
func (c *Client) convertDoc(ctx context.Context, doc chat.Document) ([]anthropic.ContentBlockParamUnion, error) {
	return convertDocument(ctx, doc, c.ID(), c.ModelOptions.ModelsDevStore())
}

func (c *Client) convertMessages(ctx context.Context, messages []chat.Message) ([]anthropic.MessageParam, error) {
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
				tr := convertToolResultBlock(&messages[j])
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

	// Add ephemeral cache to last 2 messages' last content block
	applyMessageCacheControl(anthropicMessages)

	return anthropicMessages, nil
}

// convertToolResultBlock converts a tool message to an Anthropic tool_result block.
// If the message contains image content in MultiContent, the images are included
// as image blocks within the tool_result.
func convertToolResultBlock(msg *chat.Message) anthropic.ContentBlockParamUnion {
	// If there are no images in MultiContent, use the simple text-only format.
	if !hasImageMultiContent(msg.MultiContent) {
		// tool_result must be present for every preceding tool_use; we cannot skip
		// it. Normalize whitespace-only content to empty string rather than skipping.
		content := msg.Content
		if strings.TrimSpace(content) == "" {
			content = ""
		}
		return anthropic.NewToolResultBlock(msg.ToolCallID, content, msg.IsError)
	}

	// Build content blocks with text + images for the tool result.
	var content []anthropic.ToolResultBlockParamContentUnion
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: part.Text},
			})
		case chat.MessagePartTypeImageURL:
			if part.ImageURL == nil {
				continue
			}
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
				if len(urlParts) == 2 {
					mediaType := extractMediaType(urlParts[0])
					content = append(content, anthropic.ToolResultBlockParamContentUnion{
						OfImage: &anthropic.ImageBlockParam{
							Source: anthropic.ImageBlockParamSourceUnion{
								OfBase64: &anthropic.Base64ImageSourceParam{
									Data:      urlParts[1],
									MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
								},
							},
						},
					})
				}
			}
		}
	}

	toolBlock := anthropic.ToolResultBlockParam{
		ToolUseID: msg.ToolCallID,
		Content:   content,
		IsError:   anthropic.Bool(msg.IsError),
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: &toolBlock}
}

// hasImageMultiContent returns true if the multi-content parts contain any image content.
func hasImageMultiContent(parts []chat.MessagePart) bool {
	for _, part := range parts {
		if part.Type == chat.MessagePartTypeImageURL && part.ImageURL != nil {
			return true
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
// It handles text and images (base64 and URL). File uploads are NOT supported in the non-Beta API
// and will return an error - callers should use hasFileAttachments() to route to the Beta API.
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
			// Handle base64 data URLs (legacy format)
			if strings.HasPrefix(part.ImageURL.URL, "data:") {
				urlParts := strings.SplitN(part.ImageURL.URL, ",", 2)
				if len(urlParts) == 2 {
					mediaType := extractMediaType(urlParts[0])
					base64Data := urlParts[1]

					contentBlocks = append(contentBlocks, anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{
						Data:      base64Data,
						MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
					}))
				}
			} else if strings.HasPrefix(part.ImageURL.URL, "http://") || strings.HasPrefix(part.ImageURL.URL, "https://") {
				// URL-based images
				contentBlocks = append(contentBlocks, anthropic.NewImageBlock(anthropic.URLImageSourceParam{
					URL: part.ImageURL.URL,
				}))
			}

		case chat.MessagePartTypeFile:
			if part.File == nil {
				continue
			}

			// File uploads require the Beta API - this code path should not be reached
			// if hasFileAttachments() correctly routes to createBetaStream().
			// Return a clear error if we somehow get here.
			return nil, fmt.Errorf("file attachments require the Beta API; use hasFileAttachments() to route correctly (path=%q, file_id=%q)",
				part.File.Path, part.File.FileID)

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

// createFileContentBlock creates the appropriate content block for a file based on its MIME type.
// Note: File uploads via the Files API require the Beta API. This function supports images
// (which have OfFile in the Beta API only) and documents. For non-Beta API usage with files,
// the caller should handle the conversion differently or use base64 encoding.
func createFileContentBlock(fileID, mimeType string) (anthropic.ContentBlockParamUnion, error) {
	// The standard (non-Beta) API doesn't support file references in ImageBlockParamSourceUnion
	// or DocumentBlockParamSourceUnion. Files API is Beta-only.
	// For now, we return an error directing users to use the Beta API path.
	return anthropic.ContentBlockParamUnion{}, fmt.Errorf("file uploads require the Beta API; file_id=%s, mime_type=%s", fileID, mimeType)
}

// applyMessageCacheControl adds ephemeral cache control to the last content block
// of the last 2 messages for prompt caching.
func applyMessageCacheControl(messages []anthropic.MessageParam) {
	for i := len(messages) - 1; i >= 0 && i >= len(messages)-2; i-- {
		msg := &messages[i]
		if len(msg.Content) == 0 {
			continue
		}
		lastIdx := len(msg.Content) - 1
		block := &msg.Content[lastIdx]
		cacheCtrl := anthropic.NewCacheControlEphemeralParam()
		switch {
		case block.OfText != nil:
			block.OfText.CacheControl = cacheCtrl
		case block.OfToolUse != nil:
			block.OfToolUse.CacheControl = cacheCtrl
		case block.OfToolResult != nil:
			block.OfToolResult.CacheControl = cacheCtrl
		case block.OfImage != nil:
			block.OfImage.CacheControl = cacheCtrl
		case block.OfDocument != nil:
			block.OfDocument.CacheControl = cacheCtrl
		}
	}
}

// extractSystemBlocks converts any system-role messages into Anthropic system text blocks
// to be set on the top-level MessageNewParams.System field.
func extractSystemBlocks(messages []chat.Message) []anthropic.TextBlockParam {
	var systemBlocks []anthropic.TextBlockParam
	for i := range messages {
		msg := &messages[i]
		if msg.Role != chat.MessageRoleSystem {
			continue
		}

		if len(msg.MultiContent) > 0 {
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeText {
					if txt := strings.TrimSpace(part.Text); txt != "" {
						systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: txt})
					}
				}
			}
		} else if txt := strings.TrimSpace(msg.Content); txt != "" {
			// Trim system-message content: YAML literal blocks (instruction: |) always
			// append a trailing newline, and we don't want that in API payloads.
			systemBlocks = append(systemBlocks, anthropic.TextBlockParam{
				Text: txt,
			})
		}

		if msg.CacheControl && len(systemBlocks) > 0 {
			systemBlocks[len(systemBlocks)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}

	return systemBlocks
}

func convertTools(tooles []tools.Tool) ([]anthropic.ToolUnionParam, error) {
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
	}
	anthropicTools := make([]anthropic.ToolUnionParam, len(toolParams))
	for i := range toolParams {
		anthropicTools[i] = anthropic.ToolUnionParam{OfTool: &toolParams[i]}
	}

	return anthropicTools, nil
}

// ConvertParametersToSchema converts parameters to Anthropic Schema format
func ConvertParametersToSchema(params any) (anthropic.ToolInputSchemaParam, error) {
	var schema anthropic.ToolInputSchemaParam
	if err := tools.ConvertSchema(params, &schema); err != nil {
		return anthropic.ToolInputSchemaParam{}, err
	}

	return schema, nil
}

// CleanupFiles removes all files uploaded during this session from Anthropic's storage.
func (c *Client) CleanupFiles(ctx context.Context) error {
	if c.fileManager == nil {
		return nil
	}
	return c.fileManager.CleanupAll(ctx)
}

// FileManager returns the file manager for this client, allowing external cleanup.
// Returns nil if file uploads are not supported or not initialized.
func (c *Client) FileManager() *FileManager {
	return c.fileManager
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

// anthropicContextLimit returns a reasonable default context window for Anthropic models.
// We default to 200k tokens, which is what 3.5-4.5 models support; adjust as needed over time.
func anthropicContextLimit(model string) int64 {
	_ = model
	return 200000
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
) (int64, error) {
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
