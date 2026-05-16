package dmr

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	// configureTimeout is the timeout for the model configure HTTP request.
	// This is kept short to avoid stalling client creation.
	configureTimeout = 10 * time.Second

	// connectivityTimeout is the timeout for testing DMR endpoint connectivity.
	// This is kept short to quickly detect unreachable endpoints and try fallbacks.
	connectivityTimeout = 2 * time.Second
)

// ErrNotInstalled is returned when Docker Model Runner is not installed.
var ErrNotInstalled = errors.New("docker model runner is not available\nplease install it and try again (https://docs.docker.com/ai/model-runner/get-started/)")

const (
	// dmrInferencePrefix mirrors github.com/docker/model-runner/pkg/inference.InferencePrefix.
	dmrInferencePrefix = "/engines"
	// dmrExperimentalEndpointsPrefix mirrors github.com/docker/model-runner/pkg/inference.ExperimentalEndpointsPrefix.
	dmrExperimentalEndpointsPrefix = "/exp/vDD4.40"

	// dmrDefaultPort is the default port for Docker Model Runner.
	dmrDefaultPort = "12434"
)

// Client represents an DMR client wrapper
// It implements the provider.Provider interface
type Client struct {
	base.Config

	client     openai.Client
	httpClient *http.Client
	engine     string
}

// NewClient creates a new DMR client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.ErrorContext(ctx, "DMR client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	if cfg.Provider != "dmr" {
		slog.ErrorContext(ctx, "DMR client creation failed", "error", "model type must be 'dmr'", "actual_type", cfg.Provider)
		return nil, errors.New("model type must be 'dmr'")
	}

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	// Skip docker model status query when BaseURL is explicitly provided.
	// This avoids unnecessary exec calls and speeds up tests/CI scenarios.
	var endpoint, engine string
	if cfg.BaseURL == "" && os.Getenv("MODEL_RUNNER_HOST") == "" {
		var err error
		endpoint, engine, err = getDockerModelEndpointAndEngine(ctx)
		if err != nil {
			if err.Error() == "unknown flag: --json\n\nUsage:  docker [OPTIONS] COMMAND [ARG...]\n\nRun 'docker --help' for more information" {
				slog.DebugContext(ctx, "docker model status query failed", "error", err)
				return nil, ErrNotInstalled
			}
			slog.ErrorContext(ctx, "docker model status query failed", "error", err)
		} else {
			// Auto-pull the model if needed
			if err := pullDockerModelIfNeeded(ctx, cfg.Model); err != nil {
				slog.DebugContext(ctx, "docker model pull failed", "error", err)
				return nil, err
			}
		}
	}

	baseURL, clientOptions, httpClient := resolveDMRBaseURL(ctx, cfg, endpoint)

	// Ensure we always have a non-nil HTTP client for both OpenAI adapter and direct HTTP calls (rerank).
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	clientOptions = append(clientOptions, option.WithBaseURL(baseURL), option.WithAPIKey("")) // DMR doesn't need auth

	parsed, err := parseDMRProviderOpts(engine, cfg)
	if err != nil {
		slog.ErrorContext(ctx, "DMR provider_opts invalid", "error", err, "model", cfg.Model)
		return nil, err
	}
	backendCfg := buildConfigureBackendConfig(parsed.contextSize, parsed.runtimeFlags, parsed.specOpts, parsed.llamaCpp, parsed.vllm, parsed.keepAlive)
	slog.DebugContext(ctx, "DMR provider_opts parsed",
		"model", cfg.Model,
		"engine", engine,
		"context_size", derefInt64(parsed.contextSize),
		"runtime_flags", parsed.runtimeFlags,
		"raw_runtime_flags", parsed.rawRuntimeFlags,
		"mode", derefString(parsed.mode),
		"keep_alive", derefString(parsed.keepAlive),
		"speculative_opts", parsed.specOpts,
		"llamacpp", parsed.llamaCpp,
		"vllm", parsed.vllm,
	)
	// Skip model configuration when generating titles to avoid reconfiguring the model
	// with different settings (e.g., smaller max_tokens) that would affect the main agent.
	if !globalOptions.GeneratingTitle() {
		if err := configureModel(ctx, httpClient, baseURL, cfg.Model, backendCfg, parsed.mode, parsed.rawRuntimeFlags); err != nil {
			slog.DebugContext(ctx, "model configure via API skipped or failed", "error", err)
		}
	}

	slog.DebugContext(ctx, "DMR client created successfully", "model", cfg.Model, "base_url", baseURL)

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			BaseURL:      baseURL,
		},
		client:     openai.NewClient(clientOptions...),
		httpClient: httpClient,
		engine:     engine,
	}, nil
}

// convertMessages converts chat messages to OpenAI format and merges consecutive
// system/user messages, which is needed by some local models run by DMR.
func (c *Client) convertMessages(ctx context.Context, messages []chat.Message) []openai.ChatCompletionMessageParamUnion {
	openaiMessages := oaistream.ConvertMessages(ctx, messages, c.ID(), c.ModelOptions.ModelsDevStore())
	return oaistream.MergeConsecutiveMessages(openaiMessages)
}

// CreateChatCompletionStream creates a streaming chat completion request
// It returns a stream that can be iterated over to get completion chunks
func (c *Client) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) (chat.MessageStream, error) {
	slog.DebugContext(ctx, "Creating DMR chat completion stream",
		"model", c.ModelConfig.Model,
		"message_count", len(messages),
		"tool_count", len(requestTools),
		"base_url", c.BaseURL,
	)

	if len(messages) == 0 {
		slog.ErrorContext(ctx, "DMR stream creation failed", "error", "at least one message is required")
		return nil, errors.New("at least one message is required")
	}

	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage

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

	if c.ModelConfig.MaxTokens != nil {
		params.MaxTokens = openai.Int(*c.ModelConfig.MaxTokens)
		slog.DebugContext(ctx, "DMR request configured with max tokens", "max_tokens", *c.ModelConfig.MaxTokens)
	}

	if len(requestTools) > 0 {
		slog.DebugContext(ctx, "Adding tools to DMR request", "tool_count", len(requestTools))
		toolsParam := make([]openai.ChatCompletionToolUnionParam, len(requestTools))
		for i, tool := range requestTools {
			parameters, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to convert tool parameters to DMR schema", "error", err, "tool", tool.Name)
				return nil, fmt.Errorf("failed to convert tool parameters to DMR schema for tool %s: %w", tool.Name, err)
			}

			paramsMap, ok := parameters.(map[string]any)
			if !ok {
				slog.ErrorContext(ctx, "Converted parameters is not a map", "tool", tool.Name)
				return nil, fmt.Errorf("converted parameters is not a map for tool %s", tool.Name)
			}

			// DMR requires the `description` key to be present; ensure a non-empty value
			// NOTE(krissetto): workaround, remove when fixed upstream, this shouldn't be necessary
			toolsParam[i] = openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(cmp.Or(tool.Description, "Function "+tool.Name)),
				Parameters:  paramsMap,
			})
		}
		params.Tools = toolsParam

		// Only set ParallelToolCalls when tools are present; matches OpenAI provider behavior.
		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = openai.Bool(*c.ModelConfig.ParallelToolCalls)
		}
	}

	// Collect per-request extra JSON fields. SetExtraFields replaces the map
	// wholesale, so merge all contributors before a single Set call.
	extraFields := map[string]any{}

	// NoThinking: disable reasoning at the chat-template level. llama.cpp and
	// vLLM both honor chat_template_kwargs.enable_thinking=false for Qwen3 /
	// Hermes / DeepSeek-R1 style templates; other engines ignore unknown keys.
	//
	// When the caller has also set a small MaxTokens (e.g. session title
	// generation sets max_tokens=20), raise it to noThinkingMinOutputTokens
	// so any residual reasoning tokens the engine/template still emits can't
	// starve the visible output. The nil-guard is intentional: if MaxTokens
	// is unset the caller has imposed no cap, so there is nothing to floor
	// and we leave max_tokens off the request (letting the engine use its
	// own output budget). Mirrors the OpenAI provider (see
	// pkg/model/provider/openai/client.go).
	if c.ModelOptions.NoThinking() {
		extraFields["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		if c.ModelConfig.MaxTokens != nil && *c.ModelConfig.MaxTokens < noThinkingMinOutputTokens {
			params.MaxTokens = openai.Int(noThinkingMinOutputTokens)
			slog.DebugContext(ctx, "DMR NoThinking: bumped max_tokens floor",
				"from", *c.ModelConfig.MaxTokens, "to", noThinkingMinOutputTokens)
		}
	}

	// vLLM-specific per-request fields (e.g. thinking_token_budget).
	if c.engine == engineVLLM {
		if fields := buildVLLMRequestFields(&c.ModelConfig); fields != nil {
			maps.Copy(extraFields, fields)
		}
	}

	if len(extraFields) > 0 {
		params.SetExtraFields(extraFields)
		slog.DebugContext(ctx, "DMR extra request fields applied", "fields", extraFields)
	}

	// Log the request in JSON format for debugging
	if requestJSON, err := json.Marshal(params); err == nil {
		slog.DebugContext(ctx, "DMR chat completion request", "request", string(requestJSON))
	} else {
		slog.ErrorContext(ctx, "Failed to marshal DMR request to JSON", "error", err)
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.DebugContext(ctx, "Adding structured output to DMR request", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

		params.ResponseFormat.OfJSONSchema = &openai.ResponseFormatJSONSchemaParam{
			JSONSchema: openai.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:        structuredOutput.Name,
				Description: openai.String(structuredOutput.Description),
				Schema:      jsonSchema(structuredOutput.Schema),
				Strict:      openai.Bool(structuredOutput.Strict),
			},
		}
	}

	stream := c.client.Chat.Completions.NewStreaming(ctx, params)

	slog.DebugContext(ctx, "DMR chat completion stream created successfully", "model", c.ModelConfig.Model, "base_url", c.BaseURL)
	return newStreamAdapter(stream, trackUsage), nil
}

// jsonSchema is a helper type that implements json.Marshaler for map[string]any
// This allows us to pass schema maps to the OpenAI library which expects json.Marshaler
type jsonSchema map[string]any

func (j jsonSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}
