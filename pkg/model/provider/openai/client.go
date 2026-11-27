package openai

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/rag/prompts"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client represents an OpenAI client wrapper.
// It implements the provider.Provider interface.
type Client struct {
	base.Config

	clientFn func(context.Context) (*oaistream.Client, error)

	// wsPool is initialized in NewClient when transport=websocket is configured.
	// It maintains a persistent WebSocket connection across requests.
	wsPool *wsPool

	// warnThinkingBudgetIgnoredOnce dedupes the "thinking_budget has no
	// effect on this model" diagnostic (see [Client.warnThinkingBudgetIgnored])
	// so a long-lived client warns at most once instead of on every request.
	warnThinkingBudgetIgnoredOnce sync.Once
}

// NewClient creates a new OpenAI client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.ErrorContext(ctx, "OpenAI client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	globalOptions := options.Apply(opts...)
	resolvedBaseURL := "https://api.openai.com/v1"
	if globalOptions.Gateway() == "" {
		resolvedBaseURL = cmp.Or(cfg.BaseURL, os.Getenv("OPENAI_BASE_URL"), resolvedBaseURL)
	}

	var clientFn func(context.Context) (*oaistream.Client, error)
	if gateway := globalOptions.Gateway(); gateway == "" {
		var (
			apiKey         string
			requestHeaders func(context.Context, http.Header) error
		)

		switch {
		case isChatGPTProvider(cfg):
			tokenSource := chatgptTokenSource(env, cfg.TokenKey)
			if _, err := tokenSource(ctx); err != nil {
				slog.ErrorContext(ctx, "ChatGPT client creation failed", "error", err)
				return nil, err
			}
			requestHeaders = chatgptAuthHeaderFunc(tokenSource)
		case globalOptions.TokenSource() != nil:
			tokenSource := globalOptions.TokenSource()
			if _, err := tokenSource(ctx); err != nil {
				return nil, fmt.Errorf("resolving access token: %w", err)
			}
			requestHeaders = tokenAuthHeaderFunc(tokenSource)
		case cfg.TokenKey != "":
			authToken, tokenKey := authTokenForTokenKey(ctx, cfg, env)
			if authToken == "" {
				return nil, fmt.Errorf("%s environment variable is required", strings.Join(catalog.TokenKeys(cfg.Provider, tokenKey), " or "))
			}
			apiKey = authToken
		case isCustomProvider(cfg):
			slog.DebugContext(ctx, "Custom provider with no token_key, sending requests without authentication",
				"provider", cfg.Provider, "base_url", cfg.BaseURL)
		default:
			if authToken, _ := env.Get(ctx, "OPENAI_API_KEY"); authToken != "" {
				apiKey = authToken
			} else if envToken := os.Getenv("OPENAI_API_KEY"); envToken != "" {
				apiKey = envToken
			}
		}

		queryParams := map[string]string{}
		for key, name := range catalog.Lookup(cfg.Provider).QueryParams {
			if value, ok := cfg.ProviderOpts[key].(string); ok {
				queryParams[name] = value
			}
		}

		httpClient := httpclient.NewHTTPClient(ctx)
		globalOptions.WrapTransport(ctx, httpClient)
		base.WrapOpenCodeSession(cfg, httpClient)

		client := oaistream.NewClient(oaistream.ClientConfig{
			BaseURL:        resolvedBaseURL,
			APIKey:         apiKey,
			Headers:        buildHTTPHeaders(cfg),
			QueryParams:    queryParams,
			HTTPClient:     httpClient,
			RequestHeaders: requestHeaders,
		})
		clientFn = func(context.Context) (*oaistream.Client, error) {
			return client, nil
		}
	} else {
		if err := base.VerifyDockerGatewayAuth(ctx, env, gateway); err != nil {
			slog.ErrorContext(ctx, "OpenAI client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, err
		}

		clientFn = func(ctx context.Context) (*oaistream.Client, error) {
			connection, err := base.NewGatewayClient(ctx, env, gateway, "https://api.openai.com/v1", "/v1/", cfg, &globalOptions)
			if err != nil {
				return nil, err
			}

			return oaistream.NewClient(oaistream.ClientConfig{
				BaseURL:    connection.BaseURL,
				APIKey:     connection.AuthToken,
				HTTPClient: connection.HTTPClient,
			}), nil
		}
	}

	slog.DebugContext(ctx, "OpenAI client created successfully", "model", cfg.Model)

	client := &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
			BaseURL:      resolvedBaseURL,
		},
		clientFn: clientFn,
	}

	// Pre-create the WebSocket pool when the transport is configured.
	// The pool is cheap (no connections opened until the first Stream call)
	// and eager init avoids a data race on the lazy path.
	if webSocketEnabled(cfg, &globalOptions) {
		client.wsPool = newWSPool(httpToWSURL(resolvedBaseURL), client.buildWSHeaderFn())
	}

	return client, nil
}

// Close releases resources held by the client, including any pooled WebSocket
// connections. It is safe to call Close multiple times.
func (c *Client) Close() {
	if c.wsPool != nil {
		c.wsPool.Close()
	}
}

// replaysReasoning reports whether stored assistant reasoning is replayed as
// reasoning_content on Chat Completions requests. Replay defaults on; an
// explicit provider_opts.replay_reasoning value can opt out.
func (c *Client) replaysReasoning() bool {
	return oaistream.ReplayReasoning(c.ModelConfig.ProviderOpts)
}

// endpointConfig returns ModelConfig with BaseURL set to [base.Config.Endpoint],
// so endpoint checks also see OPENAI_BASE_URL.
func (c *Client) endpointConfig() *latest.ModelConfig {
	cfg := c.ModelConfig
	cfg.BaseURL = c.Endpoint()
	return &cfg
}

// convertMessages converts chat.Message to openai.ChatCompletionMessageParamUnion
// using the shared oaistream implementation.
func (c *Client) convertMessages(ctx context.Context, messages []chat.Message) []oaistream.ChatMessage {
	// Gated here rather than inside the converter so that every path below --
	// including the prompt-cache breakpoint path, which must keep computing the
	// same body it will send -- sees the same messages.
	if !c.replaysReasoning() {
		messages = oaistream.WithoutReplayedReasoning(messages)
	}
	// Explicit prompt-cache breakpoints take priority over the consecutive-
	// message merge below. [sendsExplicitCacheBreakpoints] only admits genuine
	// OpenAI vendor endpoints — including a trusted named custom OpenAI
	// provider, whose pinned api_type=openai_chatcompletions would otherwise
	// route it into the merge path — and such endpoints tolerate multiple
	// system messages, so skipping the merge is safe. It must be skipped:
	// merging rewrites the stable message boundaries the markers pin. The
	// self-hosted case the merge exists for (openai provider + custom
	// base_url) is already excluded by the gate and still merges below.
	if sendsExplicitCacheBreakpoints(c.endpointConfig(), c.ModelOptions.OpenAIVendor()) {
		return c.convertMessagesWithCacheBreakpoints(ctx, messages)
	}
	// Coalesce consecutive same-role (system/user) messages into one for generic
	// OpenAI-compatible endpoints. docker-agent emits a separate system message
	// per source (the agent instruction plus each toolset's instructions), but
	// some such backends reject a request that carries more than one system
	// message. Two failure modes have been observed: OVHcloud's Qwen3.5 returns
	// an empty stream (#3145), while vLLM serving Qwen 3.5/3.6 fails hard with
	// "HTTP 400: System message must be at the beginning" (#2327, #3344) because
	// the model's Jinja chat template only allows a system message at index 0.
	// The DMR client already applies this merge for the same reason.
	//
	// Merging identifies a third-party/self-hosted backend, so explicit
	// prompt-cache breakpoints never reach this path: such backends don't
	// accept the OpenAI-only field, and the merge rewrites message boundaries
	// the markers would pin.
	if shouldMergeConsecutiveMessages(c.endpointConfig()) {
		converted := oaistream.ConvertMessages(ctx, messages, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
		return oaistream.MergeConsecutiveMessages(converted)
	}
	return oaistream.ConvertMessages(ctx, messages, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
}

// shouldMergeConsecutiveMessages reports whether the chat-completions request
// should coalesce consecutive same-role messages before sending.
//
// docker-agent emits one system message per source (the agent instruction plus
// each toolset's instructions). Some OpenAI-compatible backends reject more than
// one system message because the served model's Jinja chat template only allows
// a system message at index 0 (Qwen 3.5/3.6 on vLLM, #2327/#3344), or return an
// empty stream (OVHcloud Qwen3.5, #3145). Merging is a safe normalization
// (same-role content is concatenated), and the DMR client already does it.
//
// It is scoped to endpoints that plausibly front such models: explicit
// chat-completions custom providers, a self-hosted server reached by pointing
// the openai provider at a custom base_url (vLLM/SGLang), and the open-model
// host aliases declared in the catalog. The official OpenAI API and other first-party APIs are
// left untouched.
func shouldMergeConsecutiveMessages(cfg *latest.ModelConfig) bool {
	if cfg == nil {
		return false
	}
	// Custom providers pin the chat-completions api_type in ProviderOpts.
	if getAPIType(cfg) == "openai_chatcompletions" {
		return true
	}
	// A custom base_url on the openai provider means a self-hosted or
	// third-party OpenAI-compatible server (e.g. vLLM), not api.openai.com.
	if cfg.Provider == "openai" && !catalog.IsDefaultEndpoint(cfg.Provider, cfg.BaseURL) {
		return true
	}
	return catalog.Lookup(cfg.Provider).MergeConsecutiveMessages
}

// contextLimit returns this model's context window in tokens, preferring the
// declared model-level context_window (authoritative for self-hosted endpoints)
// and falling back to the models.dev catalogue. It returns 0 when the window
// cannot be determined, in which case [clampMaxTokens] leaves max_tokens
// untouched.
func (c *Client) contextLimit(ctx context.Context) int64 {
	if n := c.ModelConfig.DeclaredContextWindow(); n > 0 {
		return n
	}
	return modelinfo.ContextLimit(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), 0)
}

// clampMaxTokens caps a configured max_tokens (the per-response completion
// budget) so it cannot consume the whole context window, leaving headroom for
// the prompt. OpenAI-compatible servers such as vLLM reject a request when
// prompt_tokens + max_tokens exceeds the model's context window, so a
// max_tokens set equal to the window makes even a one-token prompt fail with a
// "maximum context length" error (issue #3387). When the window is unknown (0)
// the value is returned unchanged; a genuinely oversized prompt is then handled
// by the runtime's overflow detection and compaction, not here.
func clampMaxTokens(ctx context.Context, configured, window int64, model string) int64 {
	if window <= 0 {
		return configured
	}
	const safety = int64(1024)
	capped := max(window-safety, 1)
	if configured > capped {
		slog.WarnContext(ctx,
			"max_tokens exceeds the context window minus headroom; clamping to leave room for the prompt",
			"configured", configured,
			"clamped", capped,
			"context_window", window,
			"model", model,
			"see", "https://github.com/docker/docker-agent/issues/3387",
		)
		return capped
	}
	return configured
}

// CreateChatCompletionStream creates a streaming chat completion request
// It returns a stream that can be iterated over to get completion chunks
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	slog.DebugContext(ctx, "Creating OpenAI chat completion stream",
		"model", c.ModelConfig.Model,
		"message_count", len(messages),
		"tool_count", len(requestTools))

	// Check api_type from ProviderOpts to determine which schema to use.
	// This allows custom providers to explicitly choose the API schema.
	apiType := getAPIType(&c.ModelConfig)

	// The ChatGPT Codex backend only serves /responses; there is no
	// chat-completions endpoint to fall back to, so the provider always
	// routes there, even over an explicit api_type.
	if isChatGPTProvider(&c.ModelConfig) {
		if apiType == "openai_chatcompletions" {
			slog.DebugContext(ctx, "Ignoring api_type openai_chatcompletions: the ChatGPT backend only serves the Responses API")
		}
		return c.CreateResponseStream(ctx, messages, requestTools)
	}

	switch apiType {
	case "openai_responses":
		// Force Responses API
		slog.DebugContext(ctx, "Using Responses API", "api_type", apiType, "model", c.ModelConfig.Model)
		return c.CreateResponseStream(ctx, messages, requestTools)
	case "openai_chatcompletions":
		slog.DebugContext(ctx, "Using Chat Completions API", "api_type", apiType, "model", c.ModelConfig.Model)
	default:
		// Auto-detect based on model name. Use the Responses API for newer
		// models that require it (gpt-4.1+, o-series, gpt-5, Codex). This
		// applies to both OpenAI and GitHub Copilot, which proxies the same
		// models and rejects them on /chat/completions with a 400.
		if autoSelectsResponsesAPI(c.ModelConfig.Provider) && modelinfo.SupportsResponsesAPI(c.ModelConfig.Model) {
			slog.DebugContext(ctx, "Auto-selecting Responses API", "provider", c.ModelConfig.Provider, "model", c.ModelConfig.Model)
			return c.CreateResponseStream(ctx, messages, requestTools)
		}
	}

	if len(messages) == 0 {
		slog.ErrorContext(ctx, "OpenAI stream creation failed", "error", "at least one message is required")
		return nil, errors.New("at least one message is required")
	}

	messages = c.withClaudeSchemaInstruction(ctx, messages)
	requestTools = tools.WithoutSearchOnly(requestTools)

	trackUsage := c.TrackUsageEnabled()

	params := oaistream.ChatCompletionRequest{
		Model:       c.ModelConfig.Model,
		ServiceTier: serviceTier(c.ModelConfig.ProviderOpts),
		Messages:    c.convertMessages(ctx, messages),
		StreamOptions: &oaistream.StreamOptions{
			IncludeUsage: trackUsage,
		},
	}

	if c.ModelConfig.Temperature != nil {
		params.Temperature = new(*c.ModelConfig.Temperature)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = new(*c.ModelConfig.TopP)
	}
	if c.ModelConfig.FrequencyPenalty != nil {
		params.FrequencyPenalty = new(*c.ModelConfig.FrequencyPenalty)
	}
	if c.ModelConfig.PresencePenalty != nil {
		params.PresencePenalty = new(*c.ModelConfig.PresencePenalty)
	}

	if maxToken := c.ModelConfig.MaxTokens; maxToken != nil && *maxToken > 0 {
		effective := clampMaxTokens(ctx, *maxToken, c.contextLimit(ctx), c.ModelConfig.Model)
		if !modelinfo.SupportsResponsesAPI(c.ModelConfig.Model) {
			params.MaxTokens = new(effective)
			slog.DebugContext(ctx, "OpenAI request configured with max tokens", "max_tokens", effective, "model", c.ModelConfig.Model)
		} else {
			params.MaxCompletionTokens = new(effective)
			slog.DebugContext(ctx, "using max_completion_tokens instead of max_tokens for Responses-API models", "model", c.ModelConfig.Model)
		}
	}

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to OpenAI request", "tool_count", len(requestTools))
		toolsParam := make([]oaistream.ChatTool, len(requestTools))
		for i, tool := range requestTools {
			parameters, _, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.DebugContext(ctx, "Failed to convert tool parameters to OpenAI schema", "tool_name", tool.Name, "error", err)
				return nil, err
			}
			tools.OrderedSchemaProperties(parameters)

			toolsParam[i] = oaistream.ChatTool{
				Type: "function",
				Function: oaistream.FunctionDef{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  parameters,
				},
			}

			slog.DebugContext(ctx, "Added tool to OpenAI request", "tool_name", tool.Name)
		}
		params.Tools = toolsParam
		params.ToolChoice = "auto"

		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = new(*c.ModelConfig.ParallelToolCalls)
		}
	}

	hasThinkingOverride := c.ModelOptions.NoThinking() || c.ModelConfig.ThinkingBudget != nil
	usesOpenAI := modelinfo.UsesReasoningEffort(c.ModelConfig.Model)
	usesGemini := hasThinkingOverride && !usesOpenAI && c.usesGeminiReasoningEffort(ctx)
	switch {
	case !hasThinkingOverride:
	case !usesOpenAI && !usesGemini:
		if c.ModelConfig.ThinkingBudget != nil && !c.ModelConfig.ThinkingBudget.IsDisabled() {
			c.warnThinkingBudgetIgnored(ctx)
		}
	case len(requestTools) > 0 && modelinfo.OpenAIRejectsToolsWithReasoningEffort(c.ModelConfig.Model) && !c.chatToolsWithNoneEffort():
		if c.ModelOptions.NoThinking() || c.ModelConfig.ThinkingBudget != nil {
			slog.WarnContext(ctx, "OpenAI: dropping reasoning_effort for a tools request forced onto Chat Completions; use api_type: openai_responses for tool calling on this model", "model", c.ModelConfig.Model)
		}
	case c.ModelOptions.NoThinking():
		reasoningEffort := "low"
		if sendsRealNoneEffort(&c.ModelConfig, c.ModelOptions.OpenAIVendor()) {
			reasoningEffort = "none"
		}
		params.ReasoningEffort = reasoningEffort
		if c.ModelConfig.MaxTokens != nil && *c.ModelConfig.MaxTokens > 0 && *c.ModelConfig.MaxTokens < noThinkingMinOutputTokens {
			if !modelinfo.SupportsResponsesAPI(c.ModelConfig.Model) {
				params.MaxTokens = new(noThinkingMinOutputTokens)
			} else {
				params.MaxCompletionTokens = new(noThinkingMinOutputTokens)
			}
		}
		slog.DebugContext(ctx, "OpenAI request using reasoning effort (NoThinking)", "reasoning_effort", reasoningEffort)
	case c.ModelConfig.ThinkingBudget != nil:
		if usesGemini {
			effortStr, send, err := geminiChatReasoningEffort(c.ModelConfig.Model, c.ModelConfig.ThinkingBudget)
			if err != nil {
				slog.ErrorContext(ctx, "OpenAI request using thinking_budget failed", "error", err)
				return nil, err
			}
			if send {
				params.ReasoningEffort = effortStr
				slog.DebugContext(ctx, "OpenAI request using Gemini reasoning_effort", "reasoning_effort", effortStr)
			}
			break
		}
		effortStr, err := openAIReasoningEffort(c.ModelConfig.ThinkingBudget)
		if err != nil {
			slog.ErrorContext(ctx, "OpenAI request using thinking_budget failed", "error", err)
			return nil, err
		}
		params.ReasoningEffort = effortStr
		slog.DebugContext(ctx, "OpenAI request using thinking_budget", "reasoning_effort", effortStr)
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.DebugContext(ctx, "OpenAI request using structured output", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

		params.ResponseFormat = &oaistream.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &oaistream.ResponseFormatJSONSchema{
				Name:        structuredOutput.Name,
				Description: structuredOutput.Description,
				Schema:      jsonSchema(structuredOutput.Schema),
				Strict:      new(structuredOutput.Strict),
			},
		}
	}

	if err := applyChatProviderOpts(&params, c.ModelConfig.ProviderOpts); err != nil {
		return nil, err
	}

	if requestJSON, err := json.Marshal(params); err == nil {
		slog.DebugContext(ctx, "OpenAI chat completion request", "request", string(requestJSON))
	} else {
		slog.ErrorContext(ctx, "Failed to marshal OpenAI request to JSON", "error", err)
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create OpenAI client", "error", err)
		return nil, err
	}

	stream := client.NewChatCompletionStream(ctx, params)

	slog.DebugContext(ctx, "OpenAI chat completion stream created successfully", "model", c.ModelConfig.Model)
	return oaistream.NewStreamAdapter(stream, trackUsage), nil
}

// warnThinkingBudgetIgnored logs, once per client, that a configured and
// enabled thinking_budget has no effect because this model does not accept
// a reasoning-effort parameter at all (docker-agent#4162: previously
// silent — the setting was accepted by config validation but dropped
// without a trace here).
func (c *Client) warnThinkingBudgetIgnored(ctx context.Context) {
	c.warnThinkingBudgetIgnoredOnce.Do(func() {
		slog.WarnContext(ctx, "OpenAI: thinking_budget is configured but this model does not support a reasoning effort parameter; the setting has no effect", "model", c.ModelConfig.Model)
	})
}

func (c *Client) supportsDeferredTools() bool {
	if enabled, ok := providerutil.GetProviderOptBool(c.ModelConfig.ProviderOpts, "supports_deferred_tools"); ok {
		return enabled
	}
	return modelinfo.SupportsDeferredTools(c.ModelConfig.Provider, c.ModelConfig.Model)
}

func injectDeferredToolLoads(input []ResponseInputItem, requestTools []tools.Tool) ([]ResponseInputItem, error) {
	byCallID := make(map[string][]tools.Tool)
	for _, tool := range requestTools {
		if tool.Deferred && tool.DeferredAtToolCallID != "" {
			byCallID[tool.DeferredAtToolCallID] = append(byCallID[tool.DeferredAtToolCallID], tool)
		}
	}
	if len(byCallID) == 0 {
		return input, nil
	}

	result := make([]ResponseInputItem, 0, len(input)+2*len(byCallID))
	for _, item := range input {
		result = append(result, item)
		if item.Type != "function_call_output" {
			continue
		}
		deferred := byCallID[item.CallID]
		if len(deferred) == 0 {
			continue
		}

		loaded := make([]ResponseTool, 0, len(deferred))
		names := make([]string, 0, len(deferred))
		for _, tool := range deferred {
			parameters, strict, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				return nil, err
			}
			tools.OrderedSchemaProperties(parameters)
			loaded = append(loaded, ResponseTool{
				Type:         "function",
				Name:         tool.Name,
				Description:  tool.Description,
				Parameters:   parameters,
				Strict:       new(strict),
				DeferLoading: new(true),
			})
			names = append(names, tool.Name)
		}

		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(item.CallID+":"+strings.Join(names, ","))))
		callID := "cagent_tool_load_" + digest[:16]
		argsJSON, _ := json.Marshal(map[string]any{"query": strings.Join(names, " "), "limit": len(names)})
		result = append(result,
			ResponseInputItem{
				Type:      "tool_search_call",
				Arguments: string(argsJSON),
				CallID:    callID,
				Execution: "client",
				Status:    "completed",
			},
			ResponseInputItem{
				Type:      "tool_search_output",
				Tools:     loaded,
				CallID:    callID,
				Execution: "client",
				Status:    "completed",
			},
		)
	}
	return result, nil
}

func (c *Client) CreateResponseStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	slog.DebugContext(ctx, "Creating OpenAI responses stream", "model", c.ModelConfig.Model)

	if len(messages) == 0 {
		slog.ErrorContext(ctx, "OpenAI responses stream creation failed", "error", "at least one message is required")
		return nil, errors.New("at least one message is required")
	}

	messages = c.withClaudeSchemaInstruction(ctx, messages)
	allRequestTools := requestTools
	requestTools, hostedTools, err := c.hostedToolSearchTools(ctx, requestTools)
	if err != nil {
		return nil, err
	}

	input := c.convertMessagesToResponseInput(ctx, messages)
	input = c.filterToolSearchHistory(input, allRequestTools)
	deferredToolsEnabled := c.supportsDeferredTools()
	if deferredToolsEnabled {
		loadedInput, err := injectDeferredToolLoads(input, requestTools)
		if err != nil {
			return nil, err
		}
		input = loadedInput
	}

	params := ResponseRequest{
		Model:       c.ModelConfig.Model,
		ServiceTier: serviceTier(c.ModelConfig.ProviderOpts),
		Input:       input,
	}
	c.configureResponseState(ctx, &params, messages)

	if c.ModelConfig.Temperature != nil {
		params.Temperature = new(*c.ModelConfig.Temperature)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = new(*c.ModelConfig.TopP)
	}

	if maxToken := c.ModelConfig.MaxTokens; maxToken != nil && *maxToken > 0 {
		maxTokens := clampMaxTokens(ctx, *maxToken, c.contextLimit(ctx), c.ModelConfig.Model)
		params.MaxOutputTokens = new(maxTokens)
		slog.DebugContext(ctx, "OpenAI responses request configured with max output tokens", "max_output_tokens", maxTokens)
	}

	if len(requestTools) > 0 || len(hostedTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to OpenAI responses request", "tool_count", len(requestTools))
		toolsParam := make([]ResponseTool, 0, len(requestTools)+len(hostedTools))
		for _, tool := range requestTools {
			if deferredToolsEnabled && tool.Deferred {
				continue
			}
			parameters, strict, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.DebugContext(ctx, "Failed to convert tool parameters to OpenAI schema", "tool_name", tool.Name, "error", err)
				return nil, err
			}
			tools.OrderedSchemaProperties(parameters)

			toolsParam = append(toolsParam, ResponseTool{
				Type:        "function",
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  parameters,
				Strict:      new(strict),
			})

			if !strict {
				slog.DebugContext(ctx, "Tool not compatible with OpenAI strict mode, falling back", "tool_name", tool.Name)
			}

			slog.DebugContext(ctx, "Added tool to OpenAI responses request", "tool_name", tool.Name)
		}
		toolsParam = append(toolsParam, hostedTools...)
		if len(toolsParam) > 0 {
			params.Tools = toolsParam
			params.ToolChoice = "auto"

			if c.ModelConfig.ParallelToolCalls != nil {
				params.ParallelToolCalls = new(*c.ModelConfig.ParallelToolCalls)
			}
		}
	}

	if modelinfo.UsesReasoningEffort(c.ModelConfig.Model) {
		if c.ModelOptions.NoThinking() {
			reasoningEffort := "low"
			if sendsRealNoneEffort(&c.ModelConfig, c.ModelOptions.OpenAIVendor()) {
				reasoningEffort = "none"
			}
			params.Reasoning = &ResponseReasoning{
				Effort: reasoningEffort,
			}
			if c.ModelConfig.MaxTokens != nil && *c.ModelConfig.MaxTokens > 0 && *c.ModelConfig.MaxTokens < noThinkingMinOutputTokens {
				params.MaxOutputTokens = new(noThinkingMinOutputTokens)
			}
			slog.DebugContext(ctx, "OpenAI responses request using reasoning effort (NoThinking)", "reasoning_effort", reasoningEffort)
		} else {
			params.Reasoning = &ResponseReasoning{
				Summary: "detailed",
			}
			if c.ModelConfig.ThinkingBudget != nil {
				effortStr, err := openAIReasoningEffort(c.ModelConfig.ThinkingBudget)
				if err != nil {
					slog.ErrorContext(ctx, "OpenAI responses request using thinking_budget failed", "error", err)
					return nil, err
				}
				params.Reasoning.Effort = effortStr
				slog.DebugContext(ctx, "OpenAI responses request using thinking_budget", "reasoning_effort", effortStr)
			}
		}
	} else if c.ModelConfig.ThinkingBudget != nil && !c.ModelConfig.ThinkingBudget.IsDisabled() {
		c.warnThinkingBudgetIgnored(ctx)
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.DebugContext(ctx, "OpenAI responses request using structured output", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

		params.Text = &ResponseTextConfig{
			Format: &ResponseTextFormat{
				Type:        "json_schema",
				Name:        structuredOutput.Name,
				Description: structuredOutput.Description,
				Schema:      structuredOutput.Schema,
				Strict:      new(structuredOutput.Strict),
			},
		}
	}

	if isChatGPTProvider(&c.ModelConfig) {
		applyChatGPTResponsesPolicy(ctx, &params)
	}

	if requestJSON, err := json.Marshal(params); err == nil {
		slog.DebugContext(ctx, "OpenAI responses request", "request", redactEncryptedContent(requestJSON))
	} else {
		slog.ErrorContext(ctx, "Failed to marshal OpenAI responses request to JSON", "error", err)
	}

	transport := getTransport(&c.ModelConfig)

	switch {
	case webSocketEnabled(&c.ModelConfig, &c.ModelOptions):
		stream, err := c.createWebSocketStream(ctx, params)
		if err != nil {
			slog.WarnContext(ctx, "WebSocket stream failed, falling back to SSE", "error", err)
		} else {
			slog.DebugContext(ctx, "OpenAI responses WebSocket stream created successfully", "model", c.ModelConfig.Model)
			return c.responseAdapter(ctx, stream), nil
		}
	case transport == "websocket" && c.ModelOptions.Gateway() != "":
		slog.DebugContext(ctx, "WebSocket transport requested but Gateway is configured, using SSE",
			"model", c.ModelConfig.Model,
			"gateway", c.ModelOptions.Gateway())
	case transport == "websocket" && c.ModelOptions.TransportWrapper() != nil:
		slog.DebugContext(ctx, "WebSocket transport requested but HTTP transport wrapper is set, using SSE",
			"model", c.ModelConfig.Model)
	case transport == "websocket":
		slog.DebugContext(ctx, "WebSocket transport requested but the endpoint is OpenCode, using SSE",
			"model", c.ModelConfig.Model)
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create OpenAI client", "error", err)
		return nil, err
	}
	params.Stream = true
	sse, _, err := client.DoStream(ctx, http.MethodPost, "/responses", params, params.ExtraBody, nil) //nolint:bodyclose // closed by sseResponseStream.Close()
	var stream responseEventStream
	if err != nil {
		stream = &sseResponseStream{err: err}
	} else {
		stream = &sseResponseStream{sse: sse}
	}

	slog.DebugContext(ctx, "OpenAI responses stream created successfully", "model", c.ModelConfig.Model)
	return c.responseAdapter(ctx, stream), nil
}

func (c *Client) createWebSocketStream(
	ctx context.Context,
	params ResponseRequest,
) (responseEventStream, error) {
	if c.wsPool == nil {
		return nil, errors.New("websocket pool not initialized")
	}

	// Full-history replay must not also append to connection-local history.
	if c.preservesResponseState() {
		params.PreviousResponseID = NullableString{Set: true, Null: true}
	}
	return c.wsPool.Stream(ctx, params)
}

// buildWSHeaderFn returns a function that produces the HTTP headers needed
// for the WebSocket handshake, including the Authorization header.
// buildWSHeaderFn returns a function that produces the HTTP headers needed
// for the WebSocket handshake, including the Authorization header and any
// custom headers from provider_opts.http_headers.
func (c *Client) buildWSHeaderFn() func(ctx context.Context) (http.Header, error) {
	return func(ctx context.Context) (http.Header, error) {
		h := http.Header{}

		// Resolve the API key using the same logic as the HTTP client.
		var apiKey string
		if source := c.ModelOptions.TokenSource(); source != nil {
			var err error
			apiKey, err = source(ctx)
			if err != nil {
				return nil, err
			}
		} else if c.ModelConfig.TokenKey != "" {
			apiKey, _ = authTokenForTokenKey(ctx, &c.ModelConfig, c.Env)
		}
		if apiKey == "" {
			// Fall back to the standard OPENAI_API_KEY env var via the
			// environment provider so that secret resolution is
			// consistent with the HTTP client path.
			apiKey, _ = c.Env.Get(ctx, "OPENAI_API_KEY")
		}
		if apiKey != "" {
			h.Set("Authorization", "Bearer "+apiKey)
		}

		// Apply custom headers from provider_opts (e.g. github-copilot's
		// required Copilot-Integration-Id) and any provider-specific defaults.
		// This ensures WebSocket connections have the same headers as HTTP.
		for name, value := range buildHeaderMap(&c.ModelConfig) {
			h.Set(name, value)
		}

		return h, nil
	}
}

func authTokenForTokenKey(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) (string, string) {
	for _, key := range catalog.TokenKeys(cfg.Provider, cfg.TokenKey) {
		if token, _ := env.Get(ctx, key); token != "" {
			return token, key
		}
	}
	return "", cfg.TokenKey
}

// webSocketEnabled reports whether transport=websocket can be honoured. WebSocket
// dials bypass http.RoundTripper, so SSE is used behind a gateway, under a
// transport wrapper, and for OpenCode, whose session header needs the RoundTripper.
func webSocketEnabled(cfg *latest.ModelConfig, opts *options.ModelOptions) bool {
	return getTransport(cfg) == "websocket" &&
		opts.Gateway() == "" &&
		opts.TransportWrapper() == nil &&
		!base.IsOpenCodeProvider(cfg)
}

// getTransport returns the streaming transport preference from ProviderOpts.
// Valid values are "sse" (default) and "websocket".
func getTransport(cfg *latest.ModelConfig) string {
	if cfg == nil || cfg.ProviderOpts == nil {
		return "sse"
	}
	if t, ok := cfg.ProviderOpts["transport"].(string); ok {
		return strings.ToLower(t)
	}
	return "sse"
}

func (c *Client) convertMessagesToResponseInput(ctx context.Context, messages []chat.Message) []ResponseInputItem {
	markBreakpoints := sendsExplicitCacheBreakpoints(c.endpointConfig(), c.ModelOptions.OpenAIVendor())
	var input []ResponseInputItem
	for _, msg := range messages {
		if replay := c.replayResponse(msg); len(replay) > 0 {
			input = append(input, replay...)
			continue
		}
		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) == 0 && len(msg.MultiContent) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue
		}

		start := len(input)
		var item ResponseInputItem
		hasItem := false

		switch msg.Role {
		case chat.MessageRoleUser:
			if len(msg.MultiContent) == 0 {
				item = ResponseInputItem{
					Type:    "message",
					Role:    "user",
					Content: new(msg.Content),
				}
			} else {
				contentParts := make([]ResponseContentPart, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					switch part.Type {
					case chat.MessagePartTypeText:
						contentParts = append(contentParts, ResponseContentPart{
							Type: "input_text",
							Text: part.Text,
						})
					case chat.MessagePartTypeImageURL:
						if part.ImageURL != nil {
							detail := "auto"
							switch part.ImageURL.Detail {
							case chat.ImageURLDetailHigh:
								detail = "high"
							case chat.ImageURLDetailLow:
								detail = "low"
							}
							contentParts = append(contentParts, ResponseContentPart{
								Type:     "input_image",
								ImageURL: part.ImageURL.URL,
								Detail:   detail,
							})
						}
					case chat.MessagePartTypeDocument:
						if part.Document != nil {
							docParts, err := convertDocumentToResponseInput(ctx, *part.Document, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
							if err != nil {
								slog.WarnContext(ctx, "failed to convert document attachment", "error", err, "doc", part.Document.Name)
								continue
							}
							contentParts = append(contentParts, docParts...)
						}
					}
				}
				item = ResponseInputItem{
					Type:         "message",
					Role:         "user",
					ContentParts: contentParts,
				}
			}
			hasItem = true

		case chat.MessageRoleAssistant:
			if len(msg.ToolCalls) == 0 {
				item = ResponseInputItem{
					Type:    "message",
					Role:    "assistant",
					Content: new(msg.Content),
				}
				hasItem = true
			} else {
				if strings.TrimSpace(msg.Content) != "" {
					input = append(input, ResponseInputItem{
						Type:    "message",
						Role:    "assistant",
						Content: new(msg.Content),
					})
				}
				for _, toolCall := range msg.ToolCalls {
					if toolCall.Type == "function" {
						input = append(input, ResponseInputItem{
							Type:      "function_call",
							CallID:    toolCall.ID,
							Name:      toolCall.Function.Name,
							Arguments: toolCall.Function.Arguments,
						})
					}
				}
			}

		case chat.MessageRoleSystem:
			if len(msg.MultiContent) == 0 {
				item = ResponseInputItem{
					Type: "message",
					Role: "system",
					ContentParts: []ResponseContentPart{
						{Type: "input_text", Text: msg.Content},
					},
				}
			} else {
				contentParts := make([]ResponseContentPart, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						contentParts = append(contentParts, ResponseContentPart{
							Type: "input_text",
							Text: part.Text,
						})
					}
				}
				item = ResponseInputItem{
					Type:         "message",
					Role:         "system",
					ContentParts: contentParts,
				}
			}
			hasItem = true

		case chat.MessageRoleTool:
			outStr, outArr := c.convertToolResultOutput(ctx, msg)
			item = ResponseInputItem{
				Type:      "function_call_output",
				CallID:    msg.ToolCallID,
				Output:    outStr,
				OutputArr: outArr,
			}
			hasItem = true
		}

		if hasItem {
			input = append(input, item)
		}

		if markBreakpoints && msg.CacheControl {
			markResponseCacheBreakpoint(input[start:])
		}
	}

	pendingCalls := make(map[string]bool)
	for _, item := range input {
		if item.Type == "function_call" && item.CallID != "" {
			pendingCalls[item.CallID] = true
		}
		if item.Type == "function_call_output" && item.CallID != "" {
			delete(pendingCalls, item.CallID)
		}
	}
	for callID := range pendingCalls {
		slog.WarnContext(ctx, "Injecting placeholder output for orphaned function call", "call_id", callID)
		input = append(input, ResponseInputItem{
			Type:   "function_call_output",
			CallID: callID,
			Output: new("(no output — tool call was not executed)"),
		})
	}

	return input
}

// CreateEmbedding generates an embedding vector for the given text
func (c *Client) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	slog.DebugContext(ctx, "Creating OpenAI embedding", "model", c.ModelConfig.Model, "text_length", len(text))

	batchResult, err := c.CreateBatchEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}

	if len(batchResult.Embeddings) == 0 {
		return nil, errors.New("no embedding returned from OpenAI")
	}

	embedding := batchResult.Embeddings[0]

	slog.DebugContext(ctx, "OpenAI embedding created successfully",
		"dimension", len(embedding),
		"input_tokens", batchResult.InputTokens,
		"total_tokens", batchResult.TotalTokens)

	return &base.EmbeddingResult{
		Embedding:   embedding,
		InputTokens: batchResult.InputTokens,
		TotalTokens: batchResult.TotalTokens,
		Cost:        batchResult.Cost,
	}, nil
}

// CreateBatchEmbedding generates embedding vectors for multiple texts.
//
// OpenAI supports up to 2048 inputs per request
func (c *Client) CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error) {
	if len(texts) == 0 {
		return &base.BatchEmbeddingResult{
			Embeddings: [][]float64{},
		}, nil
	}

	const maxBatchSize = 2048
	if len(texts) > maxBatchSize {
		return nil, fmt.Errorf("batch size %d exceeds OpenAI limit of %d", len(texts), maxBatchSize)
	}

	slog.DebugContext(ctx, "Creating OpenAI batch embeddings", "model", c.ModelConfig.Model, "batch_size", len(texts))

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create OpenAI client for batch embedding", "error", err)
		return nil, err
	}

	response, err := client.CreateEmbedding(ctx, oaistream.EmbeddingRequest{
		Input: texts,
		Model: c.ModelConfig.Model,
	}, nil)
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI batch embedding request failed", "error", err)
		return nil, fmt.Errorf("failed to create batch embeddings: %w", oaistream.WrapOpenAIError(err))
	}

	if len(response.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(response.Data))
	}

	// Convert embeddings from []float32 to [][]float64
	embeddings := make([][]float64, len(response.Data))
	for i, data := range response.Data {
		embedding32 := data.Embedding
		embedding := slices.Clip(slices.Clone(embedding32))
		if embedding == nil {
			embedding = []float64{}
		}
		embeddings[i] = embedding
	}

	// Extract usage information
	inputTokens := response.Usage.PromptTokens
	totalTokens := response.Usage.TotalTokens

	// Cost calculation is handled at the strategy level using models.dev pricing
	// Provider just returns token counts

	slog.DebugContext(ctx, "OpenAI batch embeddings created successfully",
		"batch_size", len(embeddings),
		"dimension", len(embeddings[0]),
		"input_tokens", inputTokens,
		"total_tokens", totalTokens)

	return &base.BatchEmbeddingResult{
		Embeddings:  embeddings,
		InputTokens: inputTokens,
		TotalTokens: totalTokens,
		Cost:        0, // Cost calculated at strategy level
	}, nil
}

// Rerank scores documents by relevance to the query using an OpenAI chat model.
// It returns relevance scores in the same order as input documents.
func (c *Client) Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error) {
	startMsg := "OpenAI reranking request"
	if len(documents) == 0 {
		slog.DebugContext(ctx, startMsg, "model", c.ModelConfig.Model, "num_documents", 0)
		return []float64{}, nil
	}

	slog.DebugContext(ctx, startMsg,
		"model", c.ModelConfig.Model,
		"query_length", len(query),
		"num_documents", len(documents),
		"has_criteria", criteria != "")

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create OpenAI client for reranking", "error", err)
		return nil, err
	}

	// Build user prompt with query and numbered documents (including metadata)
	userPrompt := prompts.BuildRerankDocumentsPrompt(query, documents)

	// Build system prompt with OpenAI-specific JSON format instructions
	jsonFormatInstruction := `You MUST respond with ONLY valid JSON in this exact format and nothing else:
{"scores":[s0,s1,...,sN]} where there is exactly one numeric score per document in order.`
	systemPrompt := prompts.BuildRerankSystemPrompt(documents, criteria, c.ModelConfig.ProviderOpts, jsonFormatInstruction)

	params := oaistream.ChatCompletionRequest{
		Model:       c.ModelConfig.Model,
		ServiceTier: serviceTier(c.ModelConfig.ProviderOpts),
		Messages: []oaistream.ChatMessage{
			oaistream.SystemMessage(systemPrompt),
			oaistream.UserMessage(userPrompt),
		},
	}

	if c.ModelConfig.Temperature != nil {
		params.Temperature = new(*c.ModelConfig.Temperature)
	} else {
		params.Temperature = new(0.0)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = new(*c.ModelConfig.TopP)
	}
	if c.ModelConfig.FrequencyPenalty != nil {
		params.FrequencyPenalty = new(*c.ModelConfig.FrequencyPenalty)
	}
	if c.ModelConfig.PresencePenalty != nil {
		params.PresencePenalty = new(*c.ModelConfig.PresencePenalty)
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
	params.ResponseFormat = &oaistream.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &oaistream.ResponseFormatJSONSchema{
			Name:        "rerank_scores",
			Description: "Relevance scores for each document, in input order.",
			Schema:      jsonSchema(schema),
			Strict:      new(false),
		},
	}

	if err := applyChatProviderOpts(&params, c.ModelConfig.ProviderOpts); err != nil {
		return nil, err
	}

	resp, err := client.CreateChatCompletion(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI rerank request failed", "error", err)
		return nil, fmt.Errorf("openai rerank request failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, errors.New("openai rerank response contained no choices")
	}

	raw := resp.Choices[0].Message.Content
	if raw == "" {
		return nil, errors.New("openai message has no content")
	}

	scores, err := providerutil.ParseRerankScores(raw, len(documents))
	if err != nil {
		slog.ErrorContext(ctx, "Failed to parse OpenAI rerank scores", "error", err)
		return nil, err
	}

	slog.DebugContext(ctx, "OpenAI reranking complete",
		"model", c.ModelConfig.Model,
		"num_scores", len(scores))

	return scores, nil
}

// getAPIType extracts the api_type from ProviderOpts if present.
// Returns the api_type string or empty string if not set.
func getAPIType(cfg *latest.ModelConfig) string {
	if cfg == nil || cfg.ProviderOpts == nil {
		return ""
	}
	if apiType, ok := cfg.ProviderOpts["api_type"].(string); ok {
		slog.Debug("Using api_type from the provider options set in the model config", "api_type", apiType)
		return apiType
	}
	return ""
}

// isCustomProvider returns true if the config represents a custom provider
// (defined in the providers: section). Custom providers have api_type set in ProviderOpts.
func isCustomProvider(cfg *latest.ModelConfig) bool {
	return getAPIType(cfg) != ""
}

// autoSelectsResponsesAPI reports whether, absent an explicit api_type, the
// provider should auto-select the Responses API for models that require it.
//
// This covers OpenAI directly, GitHub Copilot (which proxies the same
// OpenAI models and rejects newer ones (gpt-5, Codex, ...) on the legacy
// /chat/completions endpoint with a 400), OpenCode Zen (which publishes
// the same OpenAI Responses endpoint for its GPT model lineup), and the
// ChatGPT Codex backend (which only serves /responses). Detection is
// driven by modelinfo.SupportsResponsesAPI so new models are picked up by
// naming convention rather than a hardcoded allow-list.
func autoSelectsResponsesAPI(provider string) bool {
	return catalog.Lookup(provider).AutoResponses
}

// noThinkingMinOutputTokens is the minimum output-token budget we enforce for
// reasoning models when NoThinking is set and the caller has also supplied a
// smaller MaxTokens cap. Even with low reasoning effort the model still
// produces hidden reasoning tokens that count against max_output_tokens /
// max_completion_tokens, so a tiny cap (e.g. 20) can get entirely consumed
// by reasoning and leave nothing for visible text. The floor only raises an
// explicit cap; if MaxTokens is unset the caller has imposed no cap and there
// is nothing to floor.
const noThinkingMinOutputTokens int64 = 256

// sendsRealNoneEffort reports whether the NoThinking() request-option path
// (used e.g. for title generation) should emit reasoning_effort "none"
// instead of the "low" fallback used for models/vendors without a real
// API-level off switch.
//
// gpt-5.6+ is the first OpenAI family with a genuine "none" effort, but that
// alone is not sufficient: many built-in OpenAI-compatible aliases (xai,
// mistral, ...) front a different vendor's models yet happen to name them
// with the same "gpt-5.6"-shaped strings. "none" must only reach the wire
// for a genuine OpenAI vendor endpoint: the direct openai/chatgpt/azure
// providers, a model id explicitly qualified "openai/..." (Vercel/OpenRouter-
// style gateways) — both covered by [modelinfo.IsOpenAIVendor] — or a named
// custom provider (providers: section, no explicit `provider:` override)
// whose applied config resolves to an OpenAI api_type, covered by the
// openAIVendor parameter.
//
// openAIVendor is trusted internal state, not user-controllable config: it is
// resolved once by pkg/model/provider's isOpenAIVendor (which additionally
// knows the built-in alias registry needed to tell a genuine custom provider
// apart from a known alias with an explicitly-set api_type like xai/mistral
// — a registry this package cannot import without an import cycle, since
// pkg/model/provider imports this package to build the client) and threaded
// in via options.WithOpenAIVendor, applied by the factory after
// applyProviderDefaults runs. Nothing under cfg.ProviderOpts feeds this
// decision: that map is public config a user could set directly, and doing
// so must never be able to spoof or suppress OpenAI-only wire behavior.
func sendsRealNoneEffort(cfg *latest.ModelConfig, openAIVendor bool) bool {
	if !modelinfo.OpenAISupportsNoneEffort(cfg.Model) {
		return false
	}
	return modelinfo.IsOpenAIVendor(cfg.Provider, cfg.Model) || openAIVendor
}

func (c *Client) chatToolsWithNoneEffort() bool {
	if !modelinfo.OpenAISupportsChatToolsWithNoneEffort(c.ModelConfig.Model) ||
		!sendsRealNoneEffort(&c.ModelConfig, c.ModelOptions.OpenAIVendor()) {
		return false
	}
	level, ok := c.ModelConfig.ThinkingBudget.EffortLevel()
	return c.ModelOptions.NoThinking() || (ok && level == effort.None)
}

// openAIReasoningEffort validates a ThinkingBudget effort string for the
// OpenAI API. Returns the effort string or an error.
func openAIReasoningEffort(b *latest.ThinkingBudget) (string, error) {
	l, ok := b.EffortLevel()
	if !ok {
		return "", fmt.Errorf("OpenAI reasoning models require a string thinking_budget (%s), got effort: '%s', tokens: '%d'", effort.ValidNames(), b.Effort, b.Tokens)
	}
	s, ok := effort.ForOpenAI(l)
	if !ok {
		return "", fmt.Errorf("OpenAI reasoning models require a string thinking_budget (%s), got effort: '%s', tokens: '%d'", effort.ValidNames(), b.Effort, b.Tokens)
	}
	return s, nil
}

func geminiChatReasoningEffort(modelID string, b *latest.ThinkingBudget) (string, bool, error) {
	var (
		l  effort.Level
		ok bool
	)
	if b.IsAdaptive() {
		norm := strings.ToLower(strings.TrimSpace(b.Effort))
		sub, hasSub := strings.CutPrefix(norm, "adaptive/")
		if !hasSub {
			return "", false, nil
		}
		l, ok = effort.Parse(sub)
	} else {
		l, ok = b.EffortLevel()
	}
	if !ok {
		return "", false, fmt.Errorf("gemini 3+ models require a string thinking_budget (%s), got effort: %q, tokens: %d", effort.ValidNames(), b.Effort, b.Tokens)
	}
	if l == effort.None {
		return string(effort.Low), true, nil
	}
	clamped := effort.Clamp(modelinfo.SupportedThinkingLevels("google", modelID), l)
	s, ok := effort.ForGemini(clamped)
	if !ok {
		return "", false, fmt.Errorf("gemini 3+ models require a string thinking_budget (%s), got effort: %q, tokens: %d", effort.ValidNames(), b.Effort, b.Tokens)
	}
	return s, true, nil
}

func (c *Client) usesGeminiReasoningEffort(ctx context.Context) bool {
	if !modelinfo.UsesThinkingLevel(c.ModelConfig.Model) {
		return false
	}
	return modelinfo.ResolveGeminiThinkingSupport(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), c.ModelConfig.Model)
}

// jsonSchema is a helper type that implements json.Marshaler for map[string]any
// This allows us to pass schema maps to the OpenAI library which expects json.Marshaler
type jsonSchema map[string]any

func (j jsonSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}
