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
	"net/url"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
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

	clientFn func(context.Context) (*openai.Client, error)

	// wsPool is initialized in NewClient when transport=websocket is configured.
	// It maintains a persistent WebSocket connection across requests.
	wsPool *wsPool
}

// NewClient creates a new OpenAI client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.ErrorContext(ctx, "OpenAI client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	globalOptions := options.Apply(opts...)

	var clientFn func(context.Context) (*openai.Client, error)
	if gateway := globalOptions.Gateway(); gateway == "" {
		var clientOptions []option.RequestOption

		switch {
		case isChatGPTProvider(cfg):
			// ChatGPT account (subscription) auth: a per-request middleware
			// owns the Authorization and Codex headers so token refreshes
			// apply mid-session. Fail fast with sign-in guidance when no
			// credential is available at construction time.
			tokenSource := chatgptTokenSource(env, cfg.TokenKey)
			if _, err := tokenSource(ctx); err != nil {
				slog.ErrorContext(ctx, "ChatGPT client creation failed", "error", err)
				return nil, err
			}
			// The empty API key disables the SDK's OPENAI_API_KEY fallback.
			clientOptions = append(clientOptions,
				option.WithAPIKey(""),
				option.WithMiddleware(chatgptAuthMiddleware(tokenSource)),
			)
		case cfg.TokenKey != "":
			// Explicit token_key configured - use that env var
			authToken, tokenKey := authTokenForTokenKey(ctx, cfg, env)
			if authToken == "" {
				if cfg.Provider == "github-copilot" && cfg.TokenKey == "GITHUB_TOKEN" {
					return nil, errors.New("GITHUB_TOKEN or GH_TOKEN environment variable is required")
				}
				return nil, fmt.Errorf("%s environment variable is required", tokenKey)
			}
			clientOptions = append(clientOptions, option.WithAPIKey(authToken))
		case isCustomProvider(cfg):
			// Custom provider (has api_type in ProviderOpts) without token_key - no auth
			slog.DebugContext(ctx, "Custom provider with no token_key, sending requests without authentication",
				"provider", cfg.Provider, "base_url", cfg.BaseURL)
			clientOptions = append(clientOptions, option.WithAPIKey(""))
		default:
			// No token_key configured: resolve OPENAI_API_KEY through the
			// environment provider chain instead of letting the SDK read it
			// from the OS environment. The chain resolves secret references
			// (e.g. "op://..." via the 1Password CLI) and consults extra
			// sources (Docker Desktop, ...); the SDK's os.Getenv
			// fallback would send the raw reference as the bearer token.
			if authToken, _ := env.Get(ctx, "OPENAI_API_KEY"); authToken != "" {
				clientOptions = append(clientOptions, option.WithAPIKey(authToken))
			}
			// Otherwise let the OpenAI SDK use its default behavior
			// (OPENAI_API_KEY from the OS env).
		}

		if cfg.Provider == "azure" {
			// Azure configuration
			if cfg.BaseURL != "" {
				clientOptions = append(clientOptions, option.WithBaseURL(cfg.BaseURL))
			}

			// Azure API version from provider opts
			if cfg.ProviderOpts != nil {
				if apiVersion, exists := cfg.ProviderOpts["api_version"]; exists {
					slog.DebugContext(ctx, "Setting API version", "api_version", apiVersion)
					if apiVersionStr, ok := apiVersion.(string); ok {
						clientOptions = append(clientOptions, option.WithQueryAdd("api-version", apiVersionStr))
					}
				}
			}
		} else if cfg.BaseURL != "" {
			clientOptions = append(clientOptions, option.WithBaseURL(cfg.BaseURL))
		}

		// Apply custom HTTP headers from provider_opts (e.g. github-copilot's
		// required Copilot-Integration-Id) and any provider-specific defaults.
		clientOptions = append(clientOptions, buildHeaderOptions(cfg)...)

		// Preserve full error details from non-OpenAI providers (e.g. GitHub
		// Copilot returns a bare "400 Bad Request" whose body explains the
		// actual cause); without this the SDK discards it.
		clientOptions = append(clientOptions, option.WithMiddleware(oaistream.ErrorBodyMiddleware()))

		httpClient := httpclient.NewHTTPClient(ctx)
		globalOptions.WrapTransport(ctx, httpClient)
		clientOptions = append(clientOptions, option.WithHTTPClient(httpClient))

		client := openai.NewClient(clientOptions...)
		clientFn = func(context.Context) (*openai.Client, error) {
			return &client, nil
		}
	} else {
		// When using a Gateway targeting a Docker domain, tokens are short-lived.
		// Only require and inject the Docker JWT if the gateway is a .docker.com URL.
		if err := base.VerifyDockerGatewayAuth(ctx, env, gateway); err != nil {
			slog.ErrorContext(ctx, "OpenAI client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, err
		}

		// When using a Gateway, tokens are short-lived.
		clientFn = func(ctx context.Context) (*openai.Client, error) {
			// Query a fresh auth token each time the client is used.
			authToken, err := base.GatewayAuthToken(ctx, env, gateway)
			if err != nil {
				return nil, err
			}

			url, err := url.Parse(gateway)
			if err != nil {
				return nil, fmt.Errorf("invalid gateway URL: %w", err)
			}
			baseURL := fmt.Sprintf("%s://%s%s/v1/", url.Scheme, url.Host, url.Path)

			// Configure a custom HTTP client to inject headers and query params used by the Gateway.
			httpOptions := base.GatewayHTTPOptions(url, "https://api.openai.com/v1", cfg, &globalOptions)

			gatewayHTTPClient := httpclient.NewHTTPClient(ctx, httpOptions...)
			globalOptions.WrapTransport(ctx, gatewayHTTPClient)
			clientOptions := []option.RequestOption{
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(gatewayHTTPClient),
				option.WithMiddleware(oaistream.ErrorBodyMiddleware()),
			}
			if authToken != "" {
				clientOptions = append(clientOptions, option.WithAPIKey(authToken))
			}
			client := openai.NewClient(clientOptions...)

			return &client, nil
		}
	}

	slog.DebugContext(ctx, "OpenAI client created successfully", "model", cfg.Model)

	client := &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: clientFn,
	}

	// Pre-create the WebSocket pool when the transport is configured.
	// The pool is cheap (no connections opened until the first Stream call)
	// and eager init avoids a data race on the lazy path.
	// WebSocket is also skipped when an HTTP transport wrapper is registered:
	// gorilla/websocket dials raw TCP and never calls http.RoundTripper, so the
	// wrapper cannot be applied. Fall back to SSE so the wrapper covers all calls.
	if getTransport(cfg) == "websocket" && globalOptions.Gateway() == "" && globalOptions.TransportWrapper() == nil {
		baseURL := cmp.Or(cfg.BaseURL, "https://api.openai.com/v1")
		client.wsPool = newWSPool(httpToWSURL(baseURL), client.buildWSHeaderFn())
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

// convertMessages converts chat.Message to openai.ChatCompletionMessageParamUnion
// using the shared oaistream implementation.
func (c *Client) convertMessages(ctx context.Context, messages []chat.Message) []openai.ChatCompletionMessageParamUnion {
	// Explicit prompt-cache breakpoints take priority over the consecutive-
	// message merge below. [sendsExplicitCacheBreakpoints] only admits genuine
	// OpenAI vendor endpoints — including a trusted named custom OpenAI
	// provider, whose pinned api_type=openai_chatcompletions would otherwise
	// route it into the merge path — and such endpoints tolerate multiple
	// system messages, so skipping the merge is safe. It must be skipped:
	// merging rewrites the stable message boundaries the markers pin. The
	// self-hosted case the merge exists for (openai provider + custom
	// base_url) is already excluded by the gate and still merges below.
	if sendsExplicitCacheBreakpoints(&c.ModelConfig, c.ModelOptions.OpenAIVendor()) {
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
	if shouldMergeConsecutiveMessages(&c.ModelConfig) {
		converted := oaistream.ConvertMessages(ctx, messages, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
		return oaistream.MergeConsecutiveMessages(converted)
	}
	return oaistream.ConvertMessages(ctx, messages, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
}

// openModelHostProviders are built-in OpenAI-compatible aliases that front
// arbitrary open-weight models (Qwen, GLM, Llama, DeepSeek, ...) whose
// server-side chat templates may reject a request with more than one system
// message. First-party APIs with a fixed, vendor-controlled model lineup
// (openai, mistral, xai, minimax, github-copilot, opencode) tolerate multiple
// system messages and are deliberately absent so their behavior is unchanged.
var openModelHostProviders = map[string]bool{
	"atlascloud":  true,
	"baseten":     true,
	"ovhcloud":    true,
	"openrouter":  true,
	"nebius":      true,
	"nvidia":      true,
	"cerebras":    true,
	"fireworks":   true,
	"together":    true,
	"huggingface": true,
	"vercel":      true,
	// Cloudflare Workers AI serves open-weight models directly; the AI Gateway
	// fronts them (and other providers) through one endpoint. Both plausibly
	// reach models with strict single-system-message chat templates.
	"cloudflare-workers-ai": true,
	"cloudflare-ai-gateway": true,
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
// host aliases above. The official OpenAI API and other first-party APIs are
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
	if cfg.Provider == "openai" && cfg.BaseURL != "" {
		return true
	}
	return openModelHostProviders[cfg.Provider]
}

// contextLimit returns this model's context window in tokens, preferring an
// explicit provider_opts.context_size (authoritative for self-hosted
// endpoints) and falling back to the models.dev catalogue. It returns 0 when
// the window cannot be determined, in which case [clampMaxTokens] leaves
// max_tokens untouched.
func (c *Client) contextLimit(ctx context.Context) int64 {
	if n := latest.ContextSizeFromProviderOpts(c.ModelConfig.ProviderOpts); n > 0 {
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

	trackUsage := c.TrackUsageEnabled()

	params := openai.ChatCompletionNewParams{
		Model:    c.ModelConfig.Model,
		Messages: c.convertMessages(ctx, messages),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(trackUsage),
		},
	}

	if c.ModelConfig.Temperature != nil {
		params.Temperature = openai.Float(*c.ModelConfig.Temperature)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = openai.Float(*c.ModelConfig.TopP)
	}
	if c.ModelConfig.FrequencyPenalty != nil {
		params.FrequencyPenalty = openai.Float(*c.ModelConfig.FrequencyPenalty)
	}
	if c.ModelConfig.PresencePenalty != nil {
		params.PresencePenalty = openai.Float(*c.ModelConfig.PresencePenalty)
	}

	if maxToken := c.ModelConfig.MaxTokens; maxToken != nil && *maxToken > 0 {
		effective := clampMaxTokens(ctx, *maxToken, c.contextLimit(ctx), c.ModelConfig.Model)
		if !modelinfo.SupportsResponsesAPI(c.ModelConfig.Model) {
			params.MaxTokens = openai.Int(effective)
			slog.DebugContext(ctx, "OpenAI request configured with max tokens", "max_tokens", effective, "model", c.ModelConfig.Model)
		} else {
			params.MaxCompletionTokens = openai.Int(effective)
			slog.DebugContext(ctx, "using max_completion_tokens instead of max_tokens for Responses-API models", "model", c.ModelConfig.Model)
		}
	}

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to OpenAI request", "tool_count", len(requestTools))
		toolsParam := make([]openai.ChatCompletionToolUnionParam, len(requestTools))
		for i, tool := range requestTools {
			parameters, _, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.DebugContext(ctx, "Failed to convert tool parameters to OpenAI schema", "tool_name", tool.Name, "error", err)
				return nil, err
			}

			toolsParam[i] = openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  parameters,
			})

			slog.DebugContext(ctx, "Added tool to OpenAI request", "tool_name", tool.Name)
		}
		params.Tools = toolsParam

		// Explicitly send tool_choice="auto". The OpenAI spec treats omission as
		// equivalent to "auto", but some strict OpenAI-compatible gateways
		// (notably LiteLLM) reject requests where tool_choice is missing while
		// tools are present. Sending the default value explicitly is
		// spec-compliant and preserves the model's autonomy to call tools.
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: param.NewOpt("auto"),
		}

		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = openai.Bool(*c.ModelConfig.ParallelToolCalls)
		}
	}

	// Apply thinking budget: set reasoning_effort for reasoning models (o-series, gpt-5).
	// Reasoning models always reason; omitting the param uses the default effort.
	// When NoThinking is set we still need to send an explicit effort so hidden
	// reasoning tokens don't exhaust the max_completion_tokens budget: "none" on
	// a genuine OpenAI vendor endpoint whose model has a real off switch
	// (gpt-5.6+, see [sendsRealNoneEffort]), "low" otherwise — older models
	// (o3-mini, o1) only accept low/medium/high, and "low" is the closest thing
	// to off they have.
	//
	// If the caller also supplied a small MaxTokens cap, raise it to
	// noThinkingMinOutputTokens so residual hidden reasoning can't starve
	// visible output. The nil-guard is intentional: when MaxTokens is unset
	// the caller has imposed no cap, so there is nothing to floor.
	if modelinfo.UsesReasoningEffort(c.ModelConfig.Model) {
		if c.ModelOptions.NoThinking() {
			reasoningEffort := "low"
			if sendsRealNoneEffort(&c.ModelConfig, c.ModelOptions.OpenAIVendor()) {
				reasoningEffort = "none"
			}
			params.ReasoningEffort = shared.ReasoningEffort(reasoningEffort)
			// Hidden reasoning tokens count against the output budget even
			// with low effort. Enforce a floor so visible text isn't starved.
			if c.ModelConfig.MaxTokens != nil && *c.ModelConfig.MaxTokens < noThinkingMinOutputTokens {
				if !modelinfo.SupportsResponsesAPI(c.ModelConfig.Model) {
					params.MaxTokens = openai.Int(noThinkingMinOutputTokens)
				} else {
					params.MaxCompletionTokens = openai.Int(noThinkingMinOutputTokens)
				}
			}
			slog.DebugContext(ctx, "OpenAI request using reasoning effort (NoThinking)", "reasoning_effort", reasoningEffort)
		} else if c.ModelConfig.ThinkingBudget != nil {
			effortStr, err := openAIReasoningEffort(c.ModelConfig.ThinkingBudget)
			if err != nil {
				slog.ErrorContext(ctx, "OpenAI request using thinking_budget failed", "error", err)
				return nil, err
			}
			params.ReasoningEffort = shared.ReasoningEffort(effortStr)
			slog.DebugContext(ctx, "OpenAI request using thinking_budget", "reasoning_effort", effortStr)
		}
	}

	// Apply structured output configuration
	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.DebugContext(ctx, "OpenAI request using structured output", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

		params.ResponseFormat.OfJSONSchema = &openai.ResponseFormatJSONSchemaParam{
			JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:        structuredOutput.Name,
				Description: openai.String(structuredOutput.Description),
				Schema:      jsonSchema(structuredOutput.Schema),
				Strict:      openai.Bool(structuredOutput.Strict),
			},
		}
	}

	// Log the request in JSON format for debugging
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

	// Forward sampling-related provider_opts as extra body fields.
	// This allows custom/OpenAI-compatible providers (vLLM, Ollama, etc.)
	// to receive parameters like top_k, repetition_penalty, etc.
	applySamplingProviderOpts(&params, c.ModelConfig.ProviderOpts)

	stream := client.Chat.Completions.NewStreaming(ctx, params)

	slog.DebugContext(ctx, "OpenAI chat completion stream created successfully", "model", c.ModelConfig.Model)
	return newStreamAdapter(stream, trackUsage), nil
}

func (c *Client) supportsDeferredTools() bool {
	if enabled, ok := providerutil.GetProviderOptBool(c.ModelConfig.ProviderOpts, "supports_deferred_tools"); ok {
		return enabled
	}
	return modelinfo.SupportsDeferredTools(c.ModelConfig.Provider, c.ModelConfig.Model)
}

func injectDeferredToolLoads(input []responses.ResponseInputItemUnionParam, requestTools []tools.Tool) ([]responses.ResponseInputItemUnionParam, error) {
	byCallID := make(map[string][]tools.Tool)
	for _, tool := range requestTools {
		if tool.Deferred && tool.DeferredAtToolCallID != "" {
			byCallID[tool.DeferredAtToolCallID] = append(byCallID[tool.DeferredAtToolCallID], tool)
		}
	}
	if len(byCallID) == 0 {
		return input, nil
	}

	result := make([]responses.ResponseInputItemUnionParam, 0, len(input)+2*len(byCallID))
	for _, item := range input {
		result = append(result, item)
		if item.OfFunctionCallOutput == nil {
			continue
		}
		deferred := byCallID[item.OfFunctionCallOutput.CallID]
		if len(deferred) == 0 {
			continue
		}

		loaded := make([]responses.ToolUnionParam, 0, len(deferred))
		names := make([]string, 0, len(deferred))
		for _, tool := range deferred {
			parameters, strict, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				return nil, err
			}
			loaded = append(loaded, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
				Name:         tool.Name,
				Description:  param.NewOpt(tool.Description),
				Parameters:   parameters,
				Strict:       param.NewOpt(strict),
				DeferLoading: param.NewOpt(true),
			}})
			names = append(names, tool.Name)
		}

		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(item.OfFunctionCallOutput.CallID+":"+strings.Join(names, ","))))
		callID := "cagent_tool_load_" + digest[:16]
		result = append(result,
			responses.ResponseInputItemUnionParam{OfToolSearchCall: &responses.ResponseInputItemToolSearchCallParam{
				Arguments: map[string]any{"query": strings.Join(names, " "), "limit": len(names)},
				CallID:    param.NewOpt(callID),
				Execution: "client",
				Status:    "completed",
			}},
			responses.ResponseInputItemUnionParam{OfToolSearchOutput: &responses.ResponseToolSearchOutputItemParam{
				Tools:     loaded,
				CallID:    param.NewOpt(callID),
				Execution: responses.ResponseToolSearchOutputItemParamExecutionClient,
				Status:    responses.ResponseToolSearchOutputItemParamStatusCompleted,
			}},
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

	input := c.convertMessagesToResponseInput(ctx, messages)
	deferredToolsEnabled := c.supportsDeferredTools()
	if deferredToolsEnabled {
		loadedInput, err := injectDeferredToolLoads(input, requestTools)
		if err != nil {
			return nil, err
		}
		input = loadedInput
	}

	params := responses.ResponseNewParams{
		Model: c.ModelConfig.Model,
	}
	params.Input.OfInputItemList = input

	if c.ModelConfig.Temperature != nil {
		params.Temperature = param.NewOpt(*c.ModelConfig.Temperature)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = param.NewOpt(*c.ModelConfig.TopP)
	}

	if maxToken := c.ModelConfig.MaxTokens; maxToken != nil && *maxToken > 0 {
		maxTokens := clampMaxTokens(ctx, *maxToken, c.contextLimit(ctx), c.ModelConfig.Model)
		params.MaxOutputTokens = param.NewOpt(maxTokens)
		slog.DebugContext(ctx, "OpenAI responses request configured with max output tokens", "max_output_tokens", maxTokens)
	}

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to OpenAI responses request", "tool_count", len(requestTools))
		toolsParam := make([]responses.ToolUnionParam, 0, len(requestTools))
		for _, tool := range requestTools {
			if deferredToolsEnabled && tool.Deferred {
				continue
			}
			parameters, strict, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.DebugContext(ctx, "Failed to convert tool parameters to OpenAI schema", "tool_name", tool.Name, "error", err)
				return nil, err
			}

			functionTool := &responses.FunctionToolParam{
				Name:        tool.Name,
				Description: param.NewOpt(tool.Description),
				Parameters:  parameters,
				Strict:      param.NewOpt(strict),
			}
			toolsParam = append(toolsParam, responses.ToolUnionParam{OfFunction: functionTool})

			if !strict {
				slog.DebugContext(ctx, "Tool not compatible with OpenAI strict mode, falling back", "tool_name", tool.Name)
			}

			slog.DebugContext(ctx, "Added tool to OpenAI responses request", "tool_name", tool.Name)
		}
		if len(toolsParam) > 0 {
			params.Tools = toolsParam

			// Explicitly send tool_choice="auto". See the matching comment in the
			// Chat Completions path above for rationale (LiteLLM-style gateways
			// reject requests where tool_choice is omitted).
			params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
				OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsAuto),
			}

			if c.ModelConfig.ParallelToolCalls != nil {
				params.ParallelToolCalls = param.NewOpt(*c.ModelConfig.ParallelToolCalls)
			}
		}
	}

	// Configure reasoning for models that support it (o-series, gpt-5).
	// Reasoning models always reason internally; omitting the reasoning param
	// does NOT disable reasoning — it just uses the model's default effort.
	// Those hidden reasoning tokens still count against max_output_tokens,
	// so with a small budget (e.g. title generation) the model can exhaust
	// all tokens on reasoning and return empty visible text.
	//
	// If the caller also supplied a small MaxTokens cap, raise it to
	// noThinkingMinOutputTokens so residual hidden reasoning can't starve
	// visible output. The nil-guard is intentional: when MaxTokens is unset
	// the caller has imposed no cap, so there is nothing to floor.
	if modelinfo.UsesReasoningEffort(c.ModelConfig.Model) {
		if c.ModelOptions.NoThinking() {
			// Send an explicit effort so the model spends as few output tokens
			// as possible on reasoning, leaving room for visible text: "none"
			// on a genuine OpenAI vendor endpoint whose model has a real off
			// switch (gpt-5.6+, see [sendsRealNoneEffort]), "low" otherwise —
			// older models (o3-mini, o1) only accept low/medium/high.
			reasoningEffort := "low"
			if sendsRealNoneEffort(&c.ModelConfig, c.ModelOptions.OpenAIVendor()) {
				reasoningEffort = "none"
			}
			params.Reasoning = shared.ReasoningParam{
				Effort: shared.ReasoningEffort(reasoningEffort),
			}
			// Hidden reasoning tokens count against max_output_tokens even
			// with low effort. Enforce a floor so visible text isn't starved.
			if c.ModelConfig.MaxTokens != nil && *c.ModelConfig.MaxTokens < noThinkingMinOutputTokens {
				params.MaxOutputTokens = param.NewOpt(noThinkingMinOutputTokens)
			}
			slog.DebugContext(ctx, "OpenAI responses request using reasoning effort (NoThinking)", "reasoning_effort", reasoningEffort)
		} else {
			params.Reasoning = shared.ReasoningParam{
				Summary: shared.ReasoningSummaryDetailed,
			}
			if c.ModelConfig.ThinkingBudget != nil {
				effortStr, err := openAIReasoningEffort(c.ModelConfig.ThinkingBudget)
				if err != nil {
					slog.ErrorContext(ctx, "OpenAI responses request using thinking_budget failed", "error", err)
					return nil, err
				}
				params.Reasoning.Effort = shared.ReasoningEffort(effortStr)
				slog.DebugContext(ctx, "OpenAI responses request using thinking_budget", "reasoning_effort", effortStr)
			}
		}
	}

	// Apply structured output configuration
	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.DebugContext(ctx, "OpenAI responses request using structured output", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

		params.Text.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name:        structuredOutput.Name,
			Description: param.NewOpt(structuredOutput.Description),
			Schema:      structuredOutput.Schema,
			Strict:      param.NewOpt(structuredOutput.Strict),
		}
	}

	if isChatGPTProvider(&c.ModelConfig) {
		applyChatGPTResponsesPolicy(ctx, &params)
	}

	// Log the request in JSON format for debugging
	if requestJSON, err := json.Marshal(params); err == nil {
		slog.DebugContext(ctx, "OpenAI responses request", "request", string(requestJSON))
	} else {
		slog.ErrorContext(ctx, "Failed to marshal OpenAI responses request to JSON", "error", err)
	}

	// Choose transport: WebSocket or SSE (default).
	// WebSocket is disabled when using a Gateway since most gateways don't support it.
	// WebSocket is also disabled when an HTTP transport wrapper is registered: gorilla/websocket
	// dials raw TCP and never calls http.RoundTripper, so the wrapper cannot intercept those
	// connections. Fall back to SSE so the wrapper applies to all requests.
	transport := getTransport(&c.ModelConfig)
	trackUsage := c.TrackUsageEnabled()

	switch {
	case transport == "websocket" && c.ModelOptions.Gateway() == "" && c.ModelOptions.TransportWrapper() == nil:
		stream, err := c.createWebSocketStream(ctx, params)
		if err != nil {
			slog.WarnContext(ctx, "WebSocket stream failed, falling back to SSE", "error", err)
			// Fall through to SSE below.
		} else {
			slog.DebugContext(ctx, "OpenAI responses WebSocket stream created successfully", "model", c.ModelConfig.Model)
			return newResponseStreamAdapter(stream, trackUsage), nil
		}
	case transport == "websocket" && c.ModelOptions.Gateway() != "":
		slog.DebugContext(ctx, "WebSocket transport requested but Gateway is configured, using SSE",
			"model", c.ModelConfig.Model,
			"gateway", c.ModelOptions.Gateway())
	case transport == "websocket":
		slog.DebugContext(ctx, "WebSocket transport requested but HTTP transport wrapper is set, using SSE",
			"model", c.ModelConfig.Model)
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create OpenAI client", "error", err)
		return nil, err
	}
	stream := client.Responses.NewStreaming(ctx, params)

	slog.DebugContext(ctx, "OpenAI responses stream created successfully", "model", c.ModelConfig.Model)
	return newResponseStreamAdapter(stream, trackUsage), nil
}

// createWebSocketStream sends a request over the pre-initialized WebSocket
// pool, returning a responseEventStream.
func (c *Client) createWebSocketStream(
	ctx context.Context,
	params responses.ResponseNewParams,
) (responseEventStream, error) {
	if c.wsPool == nil {
		return nil, errors.New("websocket pool not initialized")
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
		if c.ModelConfig.TokenKey != "" {
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
	token, _ := env.Get(ctx, cfg.TokenKey)
	if token == "" && cfg.Provider == "github-copilot" && cfg.TokenKey == "GITHUB_TOKEN" {
		if fallback, _ := env.Get(ctx, "GH_TOKEN"); fallback != "" {
			return fallback, "GH_TOKEN"
		}
	}
	return token, cfg.TokenKey
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

func (c *Client) convertMessagesToResponseInput(ctx context.Context, messages []chat.Message) []responses.ResponseInputItemUnionParam {
	markBreakpoints := sendsExplicitCacheBreakpoints(&c.ModelConfig, c.ModelOptions.OpenAIVendor())
	var input []responses.ResponseInputItemUnionParam
	for _, msg := range messages {
		// Skip invalid messages
		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) == 0 && len(msg.MultiContent) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue
		}

		start := len(input)
		var item responses.ResponseInputItemUnionParam

		switch msg.Role {
		case chat.MessageRoleUser:
			if len(msg.MultiContent) == 0 {
				item.OfMessage = &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: param.NewOpt(msg.Content),
					},
				}
			} else {
				// Convert multi-content for user messages
				contentParts := make([]responses.ResponseInputContentUnionParam, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					switch part.Type {
					case chat.MessagePartTypeText:
						contentParts = append(contentParts, responses.ResponseInputContentUnionParam{
							OfInputText: &responses.ResponseInputTextParam{
								Text: part.Text,
							},
						})
					case chat.MessagePartTypeImageURL:
						// Note: superseded by MessagePartTypeDocument.
						if part.ImageURL != nil {
							detail := responses.ResponseInputImageContentDetailAuto
							switch part.ImageURL.Detail {
							case chat.ImageURLDetailHigh:
								detail = responses.ResponseInputImageContentDetailHigh
							case chat.ImageURLDetailLow:
								detail = responses.ResponseInputImageContentDetailLow
							}
							contentParts = append(contentParts, responses.ResponseInputContentUnionParam{
								OfInputImage: &responses.ResponseInputImageParam{
									ImageURL: param.NewOpt(part.ImageURL.URL),
									Detail:   responses.ResponseInputImageDetail(detail),
								},
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
				item.OfInputMessage = &responses.ResponseInputItemMessageParam{
					Role:    "user",
					Content: contentParts,
				}
			}

		case chat.MessageRoleAssistant:
			if len(msg.ToolCalls) == 0 {
				// Simple assistant message
				item.OfMessage = &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleAssistant,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: param.NewOpt(msg.Content),
					},
				}
			} else {
				// Assistant message with tool calls - emit text as a separate assistant
				// message before the function calls so it is not lost.
				if strings.TrimSpace(msg.Content) != "" {
					input = append(input, responses.ResponseInputItemUnionParam{
						OfMessage: &responses.EasyInputMessageParam{
							Role: responses.EasyInputMessageRoleAssistant,
							Content: responses.EasyInputMessageContentUnionParam{
								OfString: param.NewOpt(msg.Content),
							},
						},
					})
				}
				for _, toolCall := range msg.ToolCalls {
					if toolCall.Type == "function" {
						input = append(input, responses.ResponseInputItemUnionParam{
							OfFunctionCall: &responses.ResponseFunctionToolCallParam{
								CallID:    toolCall.ID,
								Name:      toolCall.Function.Name,
								Arguments: toolCall.Function.Arguments,
							},
						})
					}
				}
				// item stays unset: everything was appended above, so the
				// common tail below appends nothing more for this message.
			}

		case chat.MessageRoleSystem:
			if len(msg.MultiContent) == 0 {
				item.OfInputMessage = &responses.ResponseInputItemMessageParam{
					Role: "system",
					Content: []responses.ResponseInputContentUnionParam{
						{
							OfInputText: &responses.ResponseInputTextParam{
								Text: msg.Content,
							},
						},
					},
				}
			} else {
				// Convert multi-content for system messages
				contentParts := make([]responses.ResponseInputContentUnionParam, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					if part.Type == chat.MessagePartTypeText {
						contentParts = append(contentParts, responses.ResponseInputContentUnionParam{
							OfInputText: &responses.ResponseInputTextParam{
								Text: part.Text,
							},
						})
					}
				}
				item.OfInputMessage = &responses.ResponseInputItemMessageParam{
					Role:    "system",
					Content: contentParts,
				}
			}

		case chat.MessageRoleTool:
			// Tool response message - convert to function call output
			item.OfFunctionCallOutput = &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: msg.ToolCallID,
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: param.NewOpt(msg.Content),
				},
			}
		}

		if item.OfMessage != nil || item.OfInputMessage != nil || item.OfFunctionCall != nil || item.OfFunctionCallOutput != nil {
			input = append(input, item)
		}

		// For tool messages with attachments, inject a follow-up user message
		// since OpenAI function call outputs only support text.
		if msg.Role == chat.MessageRoleTool && len(msg.MultiContent) > 0 {
			var attachmentParts []responses.ResponseInputContentUnionParam
			for _, part := range msg.MultiContent {
				switch part.Type {
				case chat.MessagePartTypeImageURL:
					if part.ImageURL != nil {
						detail := responses.ResponseInputImageContentDetailAuto
						switch part.ImageURL.Detail {
						case chat.ImageURLDetailHigh:
							detail = responses.ResponseInputImageContentDetailHigh
						case chat.ImageURLDetailLow:
							detail = responses.ResponseInputImageContentDetailLow
						}
						attachmentParts = append(attachmentParts, responses.ResponseInputContentUnionParam{
							OfInputImage: &responses.ResponseInputImageParam{
								ImageURL: param.NewOpt(part.ImageURL.URL),
								Detail:   responses.ResponseInputImageDetail(detail),
							},
						})
					}
				case chat.MessagePartTypeDocument:
					if part.Document != nil {
						docParts, err := convertDocumentToResponseInput(ctx, *part.Document, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride())
						if err != nil {
							slog.WarnContext(ctx, "failed to convert tool result document attachment", "error", err, "doc", part.Document.Name)
							continue
						}
						attachmentParts = append(attachmentParts, docParts...)
					}
				}
			}
			if len(attachmentParts) > 0 {
				label := responses.ResponseInputContentUnionParam{
					OfInputText: &responses.ResponseInputTextParam{
						Text: "Attached content from tool result:",
					},
				}
				allParts := append([]responses.ResponseInputContentUnionParam{label}, attachmentParts...)
				input = append(input, responses.ResponseInputItemUnionParam{
					OfInputMessage: &responses.ResponseInputItemMessageParam{
						Role:    "user",
						Content: allParts,
					},
				})
			}
		}

		// Translate the CacheControl marker into an explicit prompt-cache
		// breakpoint on the last supported content block converted from this
		// message. input[start:] restricts the scan to this message's items so
		// a filtered or content-less message never marks an unrelated one.
		if markBreakpoints && msg.CacheControl {
			markResponseCacheBreakpoint(input[start:])
		}
	}
	// Safety net: ensure every function_call has a matching function_call_output.
	// The Responses API rejects requests with orphaned function calls.
	// This can happen if tool execution was interrupted (e.g. user cancellation).
	pendingCalls := make(map[string]bool)
	for _, item := range input {
		if item.OfFunctionCall != nil {
			pendingCalls[item.OfFunctionCall.CallID] = true
		}
		if item.OfFunctionCallOutput != nil {
			delete(pendingCalls, item.OfFunctionCallOutput.CallID)
		}
	}
	for callID := range pendingCalls {
		slog.WarnContext(ctx, "Injecting placeholder output for orphaned function call", "call_id", callID)
		input = append(input, responses.ResponseInputItemUnionParam{
			OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: callID,
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: param.NewOpt("(no output — tool call was not executed)"),
				},
			},
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

	params := openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: texts,
		},
		Model: c.ModelConfig.Model,
	}

	response, err := client.Embeddings.New(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI batch embedding request failed", "error", err)
		return nil, fmt.Errorf("failed to create batch embeddings: %w", err)
	}

	if len(response.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(response.Data))
	}

	// Convert embeddings from []float32 to [][]float64
	embeddings := make([][]float64, len(response.Data))
	for i, data := range response.Data {
		embedding32 := data.Embedding
		embedding := make([]float64, len(embedding32))
		copy(embedding, embedding32)
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

	params := openai.ChatCompletionNewParams{
		Model: c.ModelConfig.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
	}

	// Apply model-level sampling settings consistently with other OpenAI calls.
	// For reranking, default temperature to 0 for deterministic scoring if not explicitly set.
	if c.ModelConfig.Temperature != nil {
		params.Temperature = openai.Float(*c.ModelConfig.Temperature)
	} else {
		params.Temperature = openai.Float(0.0)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = openai.Float(*c.ModelConfig.TopP)
	}
	if c.ModelConfig.FrequencyPenalty != nil {
		params.FrequencyPenalty = openai.Float(*c.ModelConfig.FrequencyPenalty)
	}
	if c.ModelConfig.PresencePenalty != nil {
		params.PresencePenalty = openai.Float(*c.ModelConfig.PresencePenalty)
	}

	// We intentionally do NOT set max_tokens here because newer OpenAI models
	// (e.g., gpt-4.1, o-series, gpt-5) may reject max_tokens on the
	// chat.completions endpoint, preferring max_completion_tokens instead.
	// The response is a small JSON object, so relying on model defaults is fine.

	// Use OpenAI's structured outputs to enforce a stable JSON shape:
	// { "scores": [number, ...] }
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
	params.ResponseFormat.OfJSONSchema = &openai.ResponseFormatJSONSchemaParam{
		JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
			Name:        "rerank_scores",
			Description: openai.String("Relevance scores for each document, in input order."),
			Schema:      jsonSchema(schema),
			Strict:      openai.Bool(false),
		},
	}

	applySamplingProviderOpts(&params, c.ModelConfig.ProviderOpts)

	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI rerank request failed", "error", err)
		return nil, fmt.Errorf("openai rerank request failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, errors.New("openai rerank response contained no choices")
	}

	raw, err := extractOpenAIContentAsString(resp.Choices[0].Message)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to extract OpenAI rerank content", "error", err)
		return nil, err
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

// extractOpenAIContentAsString flattens a ChatCompletion message into a single string
// by inspecting its JSON representation. This avoids depending on internal union types.
func extractOpenAIContentAsString(msg openai.ChatCompletionMessage) (string, error) {
	b, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal OpenAI message: %w", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("failed to unmarshal OpenAI message: %w", err)
	}

	content, ok := m["content"]
	if !ok || content == nil {
		return "", errors.New("openai message has no content")
	}

	// Content may be a simple string or an array of parts.
	switch v := content.(type) {
	case string:
		return v, nil
	case []any:
		var out strings.Builder
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// For text parts, Anthropic-style union uses {"type":"text","text":"..."}
			if t, _ := part["type"].(string); t == "text" {
				if txt, _ := part["text"].(string); txt != "" {
					out.WriteString(txt)
				}
			}
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("unsupported OpenAI content JSON type %T", v)
	}
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
	switch provider {
	case "openai", "github-copilot", "opencode-zen", chatgpt.ProviderName:
		return true
	}
	return false
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

// jsonSchema is a helper type that implements json.Marshaler for map[string]any
// This allows us to pass schema maps to the OpenAI library which expects json.Marshaler
type jsonSchema map[string]any

func (j jsonSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}
