package bedrock

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/auth/bearer"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client represents a Bedrock client wrapper implementing provider.Provider
type Client struct {
	base.Config

	bedrockClient    *bedrockruntime.Client
	cachingSupported bool // Cached at init time for efficiency
}

func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.ErrorContext(ctx, "Bedrock client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	if cfg.Provider != "amazon-bedrock" {
		slog.ErrorContext(ctx, "Bedrock client creation failed", "error", "model type must be 'amazon-bedrock'", "actual_type", cfg.Provider)
		return nil, errors.New("model type must be 'amazon-bedrock'")
	}

	if c5, ok := modelinfo.LookupClaude5(cfg.Model); ok && c5.BedrockMantleOnly {
		err := fmt.Errorf("amazon-bedrock: model %q is only available on Bedrock Mantle, which docker-agent does not support", cfg.Model)
		slog.ErrorContext(ctx, "Bedrock client creation failed", "error", err)
		return nil, err
	}

	if providerutil.HasProviderOpt(cfg.ProviderOpts, "fallbacks") {
		err := errors.New("amazon-bedrock: fallbacks requires the Claude API and is not available on Bedrock")
		slog.ErrorContext(ctx, "Bedrock client creation failed", "error", err)
		return nil, err
	}

	globalOptions := options.Apply(opts...)

	// Check for bearer token
	// Bearer token is optional: if not provided, falls back to standard AWS credential chain (SigV4).
	//
	// NOTE: The default credential chain does not recognize Bedrock API keys; the
	// token is wired explicitly, the way the SDK handles AWS_BEARER_TOKEN_BEDROCK
	// in the process environment.
	// See: https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys-use.html
	var bearerToken string
	if cfg.TokenKey != "" {
		bearerToken, _ = env.Get(ctx, cfg.TokenKey)
		if bearerToken == "" {
			slog.DebugContext(ctx, "Bedrock token_key configured but env var is empty, falling back to AWS credential chain",
				"token_key", cfg.TokenKey)
		}
	} else {
		bearerToken, _ = env.Get(ctx, "AWS_BEARER_TOKEN_BEDROCK")
	}
	if bearerToken == "" && bearerTokenRequired {
		return nil, errors.New("amazon-bedrock requires a bearer token (token_key or AWS_BEARER_TOKEN_BEDROCK): the AWS credential chain is not available in this build")
	}

	// Build the docker-agent HTTP client (OTel instrumentation, SSE decompression,
	// Desktop proxy support) so transport-level concerns apply to Bedrock too.
	httpClient := httpclient.NewHTTPClient(ctx)

	// Apply the transport wrapper, if registered, over the full chain.
	globalOptions.WrapTransport(ctx, httpClient)

	// Build AWS config using default credential chain
	awsCfg, err := buildAWSConfig(ctx, cfg, env)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to build AWS config", "error", err)
		return nil, fmt.Errorf("failed to build AWS config: %w", err)
	}

	// Create Bedrock Runtime client with appropriate auth
	var clientOpts []func(*bedrockruntime.Options)

	// The catalog template leaves regional and partition-specific routing to AWS.
	endpoint := getProviderOpt[string](cfg.ProviderOpts, "endpoint_url")
	if !catalog.IsDefaultEndpoint("amazon-bedrock", cfg.BaseURL) {
		endpoint = cmp.Or(endpoint, cfg.BaseURL)
	}
	if endpoint != "" {
		clientOpts = append(clientOpts, func(o *bedrockruntime.Options) {
			o.BaseEndpoint = new(endpoint)
		})
	}

	// Inject our HTTP client (which carries OTel, SSE, and any caller-registered
	// transport wrapper) into the Bedrock runtime options.
	clientOpts = append(clientOpts, func(o *bedrockruntime.Options) {
		o.HTTPClient = httpClient
	})
	if bearerToken != "" {
		slog.DebugContext(ctx, "Bedrock using bearer token authentication")
		clientOpts = append(clientOpts, func(o *bedrockruntime.Options) {
			// Anonymous credentials disable SigV4; the bearer scheme must then be
			// preferred with a real provider, or the SDK selects it with a nil one.
			o.Credentials = aws.AnonymousCredentials{}
			o.BearerAuthTokenProvider = bearer.TokenProviderFunc(func(context.Context) (bearer.Token, error) {
				return bearer.Token{Value: bearerToken}, nil
			})
			o.AuthSchemePreference = []string{"httpBearerAuth"}
		})
	}

	bedrockClient := bedrockruntime.NewFromConfig(awsCfg, clientOpts...)

	// Detect prompt caching capability at init time for efficiency.
	// Uses models.dev cache pricing as proxy for capability detection.
	cachingSupported := detectCachingSupport(ctx, cfg.Model, globalOptions.ModelsDevStore())

	slog.DebugContext(ctx, "Bedrock client created successfully",
		"model", cfg.Model,
		"region", awsCfg.Region,
		"caching_supported", cachingSupported)

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		bedrockClient:    bedrockClient,
		cachingSupported: cachingSupported,
	}, nil
}

// detectCachingSupport checks if a model supports prompt caching using models.dev data.
// Models with non-zero CacheRead/CacheWrite costs support prompt caching.
// Returns false on lookup failure (safe default for unsupported models).
func detectCachingSupport(ctx context.Context, model string, store *modelsdev.Store) bool {
	if store == nil {
		return false
	}

	id := modelsdev.NewID("amazon-bedrock", model)
	m, err := store.GetModel(ctx, id)
	if err != nil {
		slog.DebugContext(ctx, "Bedrock prompt caching disabled: model not found in models.dev",
			"model_id", id.String(), "error", err)
		return false
	}

	return m.Cost != nil && (m.Cost.CacheRead > 0 || m.Cost.CacheWrite > 0)
}

// resolveRegion picks the region from provider_opts, then AWS_REGION /
// AWS_DEFAULT_REGION, defaulting to us-east-1.
func resolveRegion(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) string {
	region := getProviderOpt[string](cfg.ProviderOpts, "region")
	if region == "" {
		region, _ = env.Get(ctx, "AWS_REGION")
	}
	if region == "" {
		region, _ = env.Get(ctx, "AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	return region
}

func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	slog.DebugContext(ctx, "Creating Bedrock chat completion stream",
		"model", c.ModelConfig.Model,
		"message_count", len(messages),
		"tool_count", len(requestTools))

	if len(messages) == 0 {
		return nil, errors.New("at least one message is required")
	}

	if err := checkReplayableThinking(c.ModelConfig.Model, c.requestThinks(), messages); err != nil {
		return nil, err
	}

	if _, ok := modelinfo.LookupClaude5(c.ModelConfig.Model); ok && providerutil.FinalAssistantPrefill(messages) {
		return nil, fmt.Errorf("%s does not accept a final assistant message (prefill)", c.ModelConfig.Model)
	}

	// Build Converse input
	input := c.buildConverseStreamInput(ctx, messages, requestTools)

	// Call ConverseStream
	output, err := c.bedrockClient.ConverseStream(ctx, input)
	if err != nil {
		slog.ErrorContext(ctx, "Bedrock ConverseStream failed", "error", err)
		return nil, wrapBedrockError(fmt.Errorf("bedrock converse stream failed: %w", err))
	}

	trackUsage := c.TrackUsageEnabled()
	return newStreamAdapter(output.GetStream(), c.ModelConfig.Model, trackUsage), nil
}

func (c *Client) buildConverseStreamInput(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) *bedrockruntime.ConverseStreamInput {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId: new(c.ModelConfig.Model),
	}

	enableCaching := c.promptCachingEnabled()

	// Convert and set messages (excluding system)
	input.Messages, input.System = convertMessages(ctx, messages, c.ID(), c.ModelOptions.ModelsDevStore(), c.CapsOverride(), enableCaching)

	// Compute thinking fields first — its presence drives the inference config.
	additionalFields := c.buildAdditionalModelRequestFields()
	if additionalFields != nil {
		input.AdditionalModelRequestFields = additionalFields
	}

	// Set inference configuration (temp/topP are suppressed when thinking is on).
	input.InferenceConfig = c.buildInferenceConfig(c.isThinkingEnabled())

	// Convert and set tools
	if len(requestTools) > 0 {
		input.ToolConfig = convertToolConfig(requestTools, enableCaching)
	}

	return input
}

func (c *Client) buildInferenceConfig(thinkingEnabled bool) *types.InferenceConfiguration {
	cfg := &types.InferenceConfiguration{}

	if c.ModelConfig.MaxTokens != nil && *c.ModelConfig.MaxTokens > 0 {
		cfg.MaxTokens = new(int32(*c.ModelConfig.MaxTokens)) //nolint:gosec // user-configured token count; realistic values fit in int32
	}

	// Temperature and TopP cannot be set when extended thinking is enabled
	// (Claude requires temperature=1.0 which is the default when thinking is on).
	// Claude 5.x rejects them in every mode.
	if _, isClaude5 := modelinfo.LookupClaude5(c.ModelConfig.Model); isClaude5 {
		if c.ModelConfig.Temperature != nil || c.ModelConfig.TopP != nil {
			slog.Debug("Bedrock: Claude 5.x rejects sampling parameters, ignoring temperature/top_p", "model", c.ModelConfig.Model)
		}
	} else if !thinkingEnabled {
		if c.ModelConfig.Temperature != nil {
			cfg.Temperature = new(float32(*c.ModelConfig.Temperature))
		}
		if c.ModelConfig.TopP != nil {
			cfg.TopP = new(float32(*c.ModelConfig.TopP))
		}
	} else if c.ModelConfig.Temperature != nil || c.ModelConfig.TopP != nil {
		slog.Debug("Bedrock extended thinking enabled, ignoring temperature/top_p settings")
	}

	return cfg
}

func (c *Client) interleavedThinkingEnabled() bool {
	// Default to true, matching the documented schema behavior.
	v, ok := c.ModelConfig.ProviderOpts["interleaved_thinking"]
	if !ok {
		return true
	}
	b, ok := v.(bool)
	if !ok {
		slog.Warn("Bedrock provider_opts type mismatch",
			"key", "interleaved_thinking",
			"expected_type", "bool",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return true
	}
	return b
}

// thinkingBudget returns the configured thinking budget. The provider
// defaults turn a configured none into nil plus WithNoThinking; Claude 5.x
// thinks by default, so it gets the explicit none back.
func (c *Client) thinkingBudget() *latest.ThinkingBudget {
	if budget := c.ModelConfig.ThinkingBudget; budget != nil {
		return budget
	}
	if _, ok := modelinfo.LookupClaude5(c.ModelConfig.Model); ok && c.ModelOptions.NoThinking() {
		return &latest.ThinkingBudget{Effort: "none"}
	}
	return nil
}

// requestThinks reports whether the request sends, or defaults to, thinking.
func (c *Client) requestThinks() bool {
	if c.thinkingBudget() == nil {
		_, isClaude5 := modelinfo.LookupClaude5(c.ModelConfig.Model)
		return isClaude5
	}
	return c.isThinkingEnabled()
}

// isThinkingEnabled returns true if a valid thinking budget is configured.
// It mirrors the validation in buildAdditionalModelRequestFields but without
// side effects (no logging), so it can safely be used to gate inference config.
func (c *Client) isThinkingEnabled() bool {
	if c.thinkingBudget() == nil {
		return false
	}
	if _, ok := c.adaptiveThinkingEffort(); ok {
		return true
	}
	tokens := c.thinkingBudget().Tokens
	if t, ok := c.thinkingBudget().EffortTokens(); ok {
		tokens = t
	}
	if tokens < 1024 {
		return false
	}
	if c.ModelConfig.MaxTokens != nil && tokens >= int(*c.ModelConfig.MaxTokens) {
		return false
	}
	return true
}

// adaptiveThinkingEffort returns the `output_config.effort` value when the
// request must use adaptive thinking (`thinking.type=adaptive`) instead of a
// token budget. This happens when the user explicitly configured "adaptive"
// (or "adaptive/<effort>"), or when the model rejects token-based thinking
// (Claude Opus 4.6+) and the configured effort level or token budget is
// transparently coerced.
//
// It has no side effects so it can be shared by isThinkingEnabled and
// buildAdditionalModelRequestFields.
//
// An empty effort with ok=true means adaptive thinking at the model's default
// effort (a token budget on a Claude 5.x model).
func (c *Client) adaptiveThinkingEffort() (string, bool) {
	budget := c.thinkingBudget()
	if budget == nil {
		return "", false
	}
	c5, isClaude5 := modelinfo.LookupClaude5(c.ModelConfig.Model)
	if budget.IsDisabled() {
		if isClaude5 && !c5.CanDisableThinking {
			return "low", true
		}
		return "", false
	}
	if e, ok := budget.AdaptiveEffort(); ok {
		return e, true
	}
	if !modelinfo.RejectsTokenThinking(c.ModelConfig.Model) {
		return "", false
	}
	if l, ok := budget.EffortLevel(); ok {
		return effort.ForAnthropic(l)
	}
	if budget.Tokens > 0 {
		if isClaude5 {
			return "", true
		}
		// Coerced token budget: adaptive's default effort.
		return "high", true
	}
	return "", false
}

func (c *Client) promptCachingEnabled() bool {
	if getProviderOpt[bool](c.ModelConfig.ProviderOpts, "disable_prompt_caching") {
		return false
	}
	return c.cachingSupported
}

// buildAdditionalModelRequestFields configures Claude's extended thinking (reasoning) mode
// and forwards supported sampling parameters from provider_opts (e.g. top_k).
func (c *Client) buildAdditionalModelRequestFields() document.Interface {
	fields := map[string]any{}

	_, isClaude5 := modelinfo.LookupClaude5(c.ModelConfig.Model)

	// Forward top_k from provider_opts (Anthropic on Bedrock supports it)
	if topK, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, "top_k"); ok && !isClaude5 {
		fields["top_k"] = topK
		slog.Debug("Bedrock provider_opts: set top_k", "value", topK)
	}

	// Configure thinking budget if present and valid
	if budget := c.thinkingBudget(); budget != nil {
		if effortStr, ok := c.adaptiveThinkingEffort(); ok {
			switch {
			case budget.Tokens > 0:
				slog.Warn("Bedrock: model rejects token-based thinking budgets; switching to adaptive thinking, the token ceiling is not enforced",
					"model", c.ModelConfig.Model,
					"thinking_budget_tokens", budget.Tokens,
					"effort", effortStr)
			case !budget.IsAdaptive():
				slog.Debug("Bedrock: model rejects token-based thinking; mapping effort level to adaptive thinking",
					"model", c.ModelConfig.Model,
					"thinking_budget", budget.Effort,
					"effort", effortStr)
			case !modelinfo.RejectsTokenThinking(c.ModelConfig.Model):
				slog.Warn("Bedrock: adaptive thinking is only supported by Claude Opus 4.6+; the request may be rejected",
					"model", c.ModelConfig.Model)
			}
			slog.Debug("Bedrock request using adaptive thinking", "effort", effortStr)
			fields["thinking"] = map[string]any{"type": "adaptive"}
			if effortStr != "" {
				fields["output_config"] = map[string]any{"effort": effortStr}
			}
		} else if budget.IsDisabled() && isClaude5 {
			fields["thinking"] = map[string]any{"type": "disabled"}
		} else {
			tokens := budget.Tokens
			if t, ok := budget.EffortTokens(); ok {
				tokens = t
			}

			valid := tokens > 0
			if valid && tokens < 1024 {
				slog.Warn("Bedrock thinking_budget below minimum (1024), ignoring", "tokens", tokens)
				valid = false
			}
			if valid && c.ModelConfig.MaxTokens != nil && tokens >= int(*c.ModelConfig.MaxTokens) {
				slog.Warn("Bedrock thinking_budget must be less than max_tokens, ignoring",
					"thinking_budget", tokens,
					"max_tokens", *c.ModelConfig.MaxTokens)
				valid = false
			}

			if valid {
				slog.Debug("Bedrock request using thinking_budget", "budget_tokens", tokens)
				fields["thinking"] = map[string]any{
					"type":          "enabled",
					"budget_tokens": tokens,
				}

				if c.interleavedThinkingEnabled() {
					fields["anthropic_beta"] = []string{"interleaved-thinking-2025-05-14"}
					slog.Debug("Bedrock request using interleaved thinking beta")
				} else {
					slog.Warn("Bedrock thinking_budget is set but interleaved_thinking is explicitly disabled; " +
						"the anthropic_beta header will not be sent, which may cause the thinking budget to be ignored")
				}
			}
		}
	}

	if len(fields) == 0 {
		return nil
	}
	return document.NewLazyDocument(fields)
}

func getProviderOpt[T any](opts map[string]any, key string) T {
	var zero T
	if opts == nil {
		return zero
	}
	v, ok := opts[key]
	if !ok {
		return zero
	}
	typed, ok := v.(T)
	if !ok {
		slog.Warn("Bedrock provider_opts type mismatch",
			"key", key,
			"expected_type", fmt.Sprintf("%T", zero),
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return zero
	}
	return typed
}
