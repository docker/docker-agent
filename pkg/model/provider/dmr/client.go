package dmr

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new DMR client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.Error("DMR client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	if cfg.Provider != "dmr" {
		slog.Error("DMR client creation failed", "error", "model type must be 'dmr'", "actual_type", cfg.Provider)
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
				slog.Debug("docker model status query failed", "error", err)
				return nil, ErrNotInstalled
			}
			slog.Error("docker model status query failed", "error", err)
		} else {
			// Auto-pull the model if needed
			if err := pullDockerModelIfNeeded(ctx, cfg.Model); err != nil {
				slog.Debug("docker model pull failed", "error", err)
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

	// Build runtime flags from ModelConfig and engine
	contextSize, providerRuntimeFlags, specOpts := parseDMRProviderOpts(cfg)
	configFlags := buildRuntimeFlagsFromModelConfig(engine, cfg)
	finalFlags, warnings := mergeRuntimeFlagsPreferUser(configFlags, providerRuntimeFlags)
	for _, w := range warnings {
		slog.Warn(w)
	}
	slog.Debug("DMR provider_opts parsed", "model", cfg.Model, "context_size", contextSize, "runtime_flags", finalFlags, "speculative_opts", specOpts, "engine", engine)
	// Skip model configuration when generating titles to avoid reconfiguring the model
	// with different settings (e.g., smaller max_tokens) that would affect the main agent.
	if !globalOptions.GeneratingTitle() {
		if err := configureModel(ctx, httpClient, baseURL, cfg.Model, contextSize, finalFlags, specOpts); err != nil {
			slog.Debug("model configure via API skipped or failed", "error", err)
		}
	}

	slog.Debug("DMR client created successfully", "model", cfg.Model, "base_url", baseURL)

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
		},
		client:     openai.NewClient(clientOptions...),
		baseURL:    baseURL,
		httpClient: httpClient,
	}, nil
}

// convertMessages converts chat messages to OpenAI format and merges consecutive
// system/user messages, which is needed by some local models run by DMR.
func convertMessages(messages []chat.Message) []openai.ChatCompletionMessageParamUnion {
	openaiMessages := oaistream.ConvertMessages(messages)
	return oaistream.MergeConsecutiveMessages(openaiMessages)
}

// CreateChatCompletionStream creates a streaming chat completion request
// It returns a stream that can be iterated over to get completion chunks
func (c *Client) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) (chat.MessageStream, error) {
	slog.Debug("Creating DMR chat completion stream",
		"model", c.ModelConfig.Model,
		"message_count", len(messages),
		"tool_count", len(requestTools),
		"base_url", c.baseURL,
	)

	if len(messages) == 0 {
		slog.Error("DMR stream creation failed", "error", "at least one message is required")
		return nil, errors.New("at least one message is required")
	}

	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage

	params := openai.ChatCompletionNewParams{
		Model:    c.ModelConfig.Model,
		Messages: convertMessages(messages),
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
		slog.Debug("DMR request configured with max tokens", "max_tokens", *c.ModelConfig.MaxTokens)
	}

	if len(requestTools) > 0 {
		slog.Debug("Adding tools to DMR request", "tool_count", len(requestTools))
		toolsParam := make([]openai.ChatCompletionToolUnionParam, len(requestTools))
		for i, tool := range requestTools {
			parameters, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.Error("Failed to convert tool parameters to DMR schema", "error", err, "tool", tool.Name)
				return nil, fmt.Errorf("failed to convert tool parameters to DMR schema for tool %s: %w", tool.Name, err)
			}

			paramsMap, ok := parameters.(map[string]any)
			if !ok {
				slog.Error("Converted parameters is not a map", "tool", tool.Name)
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

		// Apply tool_choice from agent config
		if toolChoice := c.ModelOptions.ToolChoice(); toolChoice != "" {
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.Opt(toolChoice),
			}
			slog.Debug("DMR request using tool_choice", "tool_choice", toolChoice)
		}
	}

	// Log the request in JSON format for debugging
	if requestJSON, err := json.Marshal(params); err == nil {
		slog.Debug("DMR chat completion request", "request", string(requestJSON))
	} else {
		slog.Error("Failed to marshal DMR request to JSON", "error", err)
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.Debug("Adding structured output to DMR request", "structured_output", structuredOutput)

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

	slog.Debug("DMR chat completion stream created successfully", "model", c.ModelConfig.Model, "base_url", c.baseURL)
	return newStreamAdapter(stream, trackUsage), nil
}

// jsonSchema is a helper type that implements json.Marshaler for map[string]any
// This allows us to pass schema maps to the OpenAI library which expects json.Marshaler
type jsonSchema map[string]any

func (j jsonSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}
