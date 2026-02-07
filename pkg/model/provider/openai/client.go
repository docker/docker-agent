package openai

import (
	"cmp"
	"context"
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
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/prompts"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client represents an OpenAI client wrapper
// It implements the provider.Provider interface
type Client struct {
	base.Config
	clientFn func(context.Context) (*openai.Client, error)
}

// NewClient creates a new OpenAI client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		slog.Error("OpenAI client creation failed", "error", "model configuration is required")
		return nil, errors.New("model configuration is required")
	}

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	var clientFn func(context.Context) (*openai.Client, error)
	if gateway := globalOptions.Gateway(); gateway == "" {
		var clientOptions []option.RequestOption

		if cfg.TokenKey != "" {
			// Explicit token_key configured - use that env var
			authToken, _ := env.Get(ctx, cfg.TokenKey)
			if authToken == "" {
				return nil, fmt.Errorf("%s environment variable is required", cfg.TokenKey)
			}
			clientOptions = append(clientOptions, option.WithAPIKey(authToken))
		} else if !isCustomProvider(cfg) {
			// Not a custom provider - use default OpenAI behavior (OPENAI_API_KEY from env)
			// The OpenAI SDK will automatically look for OPENAI_API_KEY if no key is set
		} else {
			// Custom provider without token_key - prevent SDK from using OPENAI_API_KEY env var
			// We need to explicitly set the API key to prevent the SDK from reading OPENAI_API_KEY
			// but we don't want to send an Authorization header. The SDK doesn't send the header
			// if we use option.WithAPIKey with a specific marker value and then remove it via middleware.
			slog.Debug("Custom provider with no token_key, disabling OpenAI SDK authentication",
				"provider", cfg.Provider, "base_url", cfg.BaseURL)

			// Use a custom HTTP client that removes the Authorization header
			clientOptions = append(clientOptions, option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
				// Remove Authorization header for custom providers without token_key
				req.Header.Del("Authorization")
				return next(req)
			}))
		}
		// Otherwise let the OpenAI SDK use its default behavior (OPENAI_API_KEY from env)

		if cfg.Provider == "azure" {
			// Azure configuration
			if cfg.BaseURL != "" {
				clientOptions = append(clientOptions, option.WithBaseURL(cfg.BaseURL))
			}

			// Azure API version from provider opts
			if cfg.ProviderOpts != nil {
				if apiVersion, exists := cfg.ProviderOpts["api_version"]; exists {
					slog.Debug("Setting API version", "api_version", apiVersion)
					if apiVersionStr, ok := apiVersion.(string); ok {
						clientOptions = append(clientOptions, option.WithQueryAdd("api-version", apiVersionStr))
					}
				}
			}
		} else if cfg.BaseURL != "" {
			clientOptions = append(clientOptions, option.WithBaseURL(cfg.BaseURL))
		}


		// Apply custom headers from provider config if present
		if cfg.ProviderOpts != nil {
			if headers, exists := cfg.ProviderOpts["headers"]; exists {
				// Handle both map[string]string and map[interface{}]interface{} from YAML parsing
				headersMap := make(map[string]string)

				switch h := headers.(type) {
				case map[string]string:
					// Direct map[string]string - use as-is
					headersMap = h
				case map[interface{}]interface{}:
					// YAML parsed as map[interface{}]interface{} - convert
					for k, v := range h {
						keyStr, okKey := k.(string)
						valStr, okVal := v.(string)
						if !okKey || !okVal {
							slog.Error("Invalid header key/value type",
								"key_type", fmt.Sprintf("%T", k),
								"value_type", fmt.Sprintf("%T", v),
								"provider", cfg.Provider)
							return nil, fmt.Errorf("invalid header key/value type: key=%T, value=%T", k, v)
						}
						headersMap[keyStr] = valStr
					}
				default:
					slog.Error("Invalid headers configuration - expected map[string]string or map[interface{}]interface{}",
						"type", fmt.Sprintf("%T", headers),
						"provider", cfg.Provider)
					return nil, fmt.Errorf("invalid headers configuration: expected map[string]string, got %T", headers)
				}

				if len(headersMap) > 0 {
					slog.Debug("Applying custom headers", "count", len(headersMap), "provider", cfg.Provider)
					for key, value := range headersMap {
						// Expand environment variables in header values (e.g., ${VAR_NAME})
						expandedValue, err := environment.Expand(ctx, value, env)
						if err != nil {
							slog.Error("Failed to expand environment variable in header",
								"header", key,
								"value", value,
								"error", err,
								"provider", cfg.Provider)
							return nil, fmt.Errorf("expanding header %s: %w", key, err)
						}
						clientOptions = append(clientOptions, option.WithHeader(key, expandedValue))
						slog.Debug("Applied custom header",
							"header", key,
							"provider", cfg.Provider)
					}
				}
			}
		}

		httpClient := httpclient.NewHTTPClient()
		clientOptions = append(clientOptions, option.WithHTTPClient(httpClient))

		client := openai.NewClient(clientOptions...)
		clientFn = func(context.Context) (*openai.Client, error) {
			return &client, nil
		}
	} else {
		// Fail fast if Docker Desktop's auth token isn't available
		if token, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); token == "" {
			slog.Error("OpenAI client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, errors.New("sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
		}

		// When using a Gateway, tokens are short-lived.
		clientFn = func(ctx context.Context) (*openai.Client, error) {
			// Query a fresh auth token each time the client is used
			authToken, _ := env.Get(ctx, environment.DockerDesktopTokenEnv)
			if authToken == "" {
				return nil, errors.New("failed to get Docker Desktop token for Gateway")
			}

			url, err := url.Parse(gateway)
			if err != nil {
				return nil, fmt.Errorf("invalid gateway URL: %w", err)
			}
			baseURL := fmt.Sprintf("%s://%s%s/v1/", url.Scheme, url.Host, url.Path)

			// Configure a custom HTTP client to inject headers and query params used by the Gateway.
			httpOptions := []httpclient.Opt{
				httpclient.WithProxiedBaseURL(cmp.Or(cfg.BaseURL, "https://api.openai.com/v1")),
				httpclient.WithProvider(cfg.Provider),
				httpclient.WithModel(cfg.Model),
				httpclient.WithModelName(cfg.Name),
				httpclient.WithQuery(url.Query()),
			}
			if globalOptions.GeneratingTitle() {
				httpOptions = append(httpOptions, httpclient.WithHeader("X-Cagent-GeneratingTitle", "1"))
			}

			client := openai.NewClient(
				option.WithAPIKey(authToken),
				option.WithBaseURL(baseURL),
				option.WithHTTPClient(httpclient.NewHTTPClient(httpOptions...)),
				option.WithMiddleware(oaistream.ErrorBodyMiddleware()),
			)

			return &client, nil
		}
	}

	slog.Debug("OpenAI client created successfully", "model", cfg.Model)

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: clientFn,
	}, nil
}

// convertMessages converts chat.Message to openai.ChatCompletionMessageParamUnion
// using the shared oaistream implementation.
func convertMessages(messages []chat.Message) []openai.ChatCompletionMessageParamUnion {
	return oaistream.ConvertMessages(messages)
}

// CreateChatCompletionStream creates a streaming chat completion request
// It returns a stream that can be iterated over to get completion chunks
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	slog.Debug("Creating OpenAI chat completion stream",
		"model", c.ModelConfig.Model,
		"message_count", len(messages),
		"tool_count", len(requestTools))

	// Check api_type from ProviderOpts to determine which schema to use.
	// This allows custom providers to explicitly choose the API schema.
	apiType := getAPIType(&c.ModelConfig)

	switch apiType {
	case "openai_responses":
		// Force Responses API
		slog.Debug("Using Responses API", "api_type", apiType, "model", c.ModelConfig.Model)
		return c.CreateResponseStream(ctx, messages, requestTools)
	case "openai_chatcompletions":
		slog.Debug("Using Chat Completions API", "api_type", apiType, "model", c.ModelConfig.Model)
	default:
		// Auto-detect based on model name for OpenAI provider
		// Use Responses API for newer models that support it (gpt-4.1+, o-series, gpt-5)
		if c.ModelConfig.Provider == "openai" && isResponsesModel(c.ModelConfig.Model) {
			slog.Debug("Auto-selecting Responses API", "model", c.ModelConfig.Model)
			return c.CreateResponseStream(ctx, messages, requestTools)
		}
	}

	if len(messages) == 0 {
		slog.Error("OpenAI stream creation failed", "error", "at least one message is required")
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

	if maxToken := c.ModelConfig.MaxTokens; maxToken != nil && *maxToken > 0 {
		if !isResponsesModel(c.ModelConfig.Model) {
			params.MaxTokens = openai.Int(*maxToken)
			slog.Debug("OpenAI request configured with max tokens", "max_tokens", *maxToken, "model", c.ModelConfig.Model)
		} else {
			params.MaxCompletionTokens = openai.Int(*maxToken)
			slog.Debug("using max_completion_tokens instead of max_tokens for Responses-API models", "model", c.ModelConfig.Model)
		}
	}

	if len(requestTools) > 0 {
		slog.Debug("Adding tools to OpenAI request", "tool_count", len(requestTools))
		toolsParam := make([]openai.ChatCompletionToolUnionParam, len(requestTools))
		for i, tool := range requestTools {
			parameters, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.Debug("Failed to convert tool parameters to OpenAI schema", "tool_name", tool.Name, "error", err)
				return nil, err
			}

			toolsParam[i] = openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  parameters,
			})

			slog.Debug("Added tool to OpenAI request", "tool_name", tool.Name)
		}
		params.Tools = toolsParam

		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = openai.Bool(*c.ModelConfig.ParallelToolCalls)
		}
	}

	// Apply thinking budget: set reasoning_effort parameter
	if c.ModelConfig.ThinkingBudget != nil {
		effort, err := getOpenAIReasoningEffort(&c.ModelConfig)
		if err != nil {
			slog.Error("OpenAI request using thinking_budget failed", "error", err)
			return nil, err
		}
		params.ReasoningEffort = shared.ReasoningEffort(effort)
		slog.Debug("OpenAI request using thinking_budget", "reasoning_effort", effort)
	}

	// Apply structured output configuration
	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.Debug("OpenAI request using structured output", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

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
		slog.Debug("OpenAI chat completion request", "request", string(requestJSON))
	} else {
		slog.Error("Failed to marshal OpenAI request to JSON", "error", err)
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create OpenAI client", "error", err)
		return nil, err
	}

	stream := client.Chat.Completions.NewStreaming(ctx, params)

	slog.Debug("OpenAI chat completion stream created successfully", "model", c.ModelConfig.Model)
	return newStreamAdapter(stream, trackUsage), nil
}

func (c *Client) CreateResponseStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	slog.Debug("Creating OpenAI responses stream", "model", c.ModelConfig.Model)

	if len(messages) == 0 {
		slog.Error("OpenAI responses stream creation failed", "error", "at least one message is required")
		return nil, errors.New("at least one message is required")
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create OpenAI client", "error", err)
		return nil, err
	}

	input := convertMessagesToResponseInput(messages)

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
		maxTokens := *maxToken

		// Reasoning models consume output tokens on internal reasoning even when
		// thinking is explicitly disabled. Bump a small budget so the model has
		// headroom for both reasoning and actual text output.
		thinkingEnabled := c.ModelOptions.Thinking() == nil || *c.ModelOptions.Thinking()
		if isOpenAIReasoningModel(c.ModelConfig.Model) && !thinkingEnabled && maxTokens < 200 {
			slog.Debug("Bumping max_output_tokens for reasoning model with thinking disabled",
				"model", c.ModelConfig.Model, "original", maxTokens, "adjusted", 200)
			maxTokens = 200
		}

		params.MaxOutputTokens = param.NewOpt(maxTokens)
		slog.Debug("OpenAI responses request configured with max output tokens", "max_output_tokens", maxTokens)
	}

	if len(requestTools) > 0 {
		slog.Debug("Adding tools to OpenAI responses request", "tool_count", len(requestTools))
		toolsParam := make([]responses.ToolUnionParam, len(requestTools))
		for i, tool := range requestTools {
			parameters, err := ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				slog.Debug("Failed to convert tool parameters to OpenAI schema", "tool_name", tool.Name, "error", err)
				return nil, err
			}

			toolsParam[i] = responses.ToolUnionParam{
				OfFunction: &responses.FunctionToolParam{
					Name:        tool.Name,
					Description: param.NewOpt(tool.Description),
					Parameters:  parameters,
					Strict:      param.NewOpt(true),
				},
			}

			slog.Debug("Added tool to OpenAI responses request", "tool_name", tool.Name)
		}
		params.Tools = toolsParam

		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = param.NewOpt(*c.ModelConfig.ParallelToolCalls)
		}
	}

	// Configure reasoning for models that support it (o-series, gpt-5)
	// Request detailed reasoning summary to get thinking traces for reasoning models
	// Skip reasoning configuration entirely if thinking is explicitly disabled (via /think command)
	thinkingEnabled := c.ModelOptions.Thinking() == nil || *c.ModelOptions.Thinking()
	if isOpenAIReasoningModel(c.ModelConfig.Model) && thinkingEnabled {
		params.Reasoning = shared.ReasoningParam{
			Summary: shared.ReasoningSummaryDetailed,
		}
		// Apply thinking budget as reasoning effort if configured
		if c.ModelConfig.ThinkingBudget != nil {
			effort, err := getOpenAIReasoningEffort(&c.ModelConfig)
			if err != nil {
				slog.Error("OpenAI responses request using thinking_budget failed", "error", err)
				return nil, err
			}
			params.Reasoning.Effort = shared.ReasoningEffort(effort)
			slog.Debug("OpenAI responses request using thinking_budget", "reasoning_effort", effort)
		}
		slog.Debug("OpenAI responses request configured with reasoning summary", "model", c.ModelConfig.Model, "summary", "detailed")
	}

	// Apply structured output configuration
	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		slog.Debug("OpenAI responses request using structured output", "name", structuredOutput.Name, "strict", structuredOutput.Strict)

		params.Text.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name:        structuredOutput.Name,
			Description: param.NewOpt(structuredOutput.Description),
			Schema:      structuredOutput.Schema,
			Strict:      param.NewOpt(structuredOutput.Strict),
		}
	}

	// Log the request in JSON format for debugging
	if requestJSON, err := json.Marshal(params); err == nil {
		slog.Debug("OpenAI responses request", "request", string(requestJSON))
	} else {
		slog.Error("Failed to marshal OpenAI responses request to JSON", "error", err)
	}

	stream := client.Responses.NewStreaming(ctx, params)

	slog.Debug("OpenAI responses stream created successfully", "model", c.ModelConfig.Model)
	return newResponseStreamAdapter(stream, c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage), nil
}

func convertMessagesToResponseInput(messages []chat.Message) []responses.ResponseInputItemUnionParam {
	var input []responses.ResponseInputItemUnionParam
	for _, msg := range messages {
		// Skip invalid messages
		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) == 0 && len(msg.MultiContent) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue
		}

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
				// Assistant message with tool calls - convert to response input item with function calls
				for _, toolCall := range msg.ToolCalls {
					if toolCall.Type == "function" {
						funcCallItem := responses.ResponseInputItemUnionParam{
							OfFunctionCall: &responses.ResponseFunctionToolCallParam{
								CallID:    toolCall.ID,
								Name:      toolCall.Function.Name,
								Arguments: toolCall.Function.Arguments,
							},
						}
						input = append(input, funcCallItem)
					}
				}
				continue // Don't add the assistant message itself
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

		// For tool messages with image content, inject a follow-up user message
		// with the images since OpenAI function call outputs only support text.
		if msg.Role == chat.MessageRoleTool && len(msg.MultiContent) > 0 {
			var imageParts []responses.ResponseInputContentUnionParam
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeImageURL && part.ImageURL != nil {
					detail := responses.ResponseInputImageContentDetailAuto
					switch part.ImageURL.Detail {
					case chat.ImageURLDetailHigh:
						detail = responses.ResponseInputImageContentDetailHigh
					case chat.ImageURLDetailLow:
						detail = responses.ResponseInputImageContentDetailLow
					}
					imageParts = append(imageParts, responses.ResponseInputContentUnionParam{
						OfInputImage: &responses.ResponseInputImageParam{
							ImageURL: param.NewOpt(part.ImageURL.URL),
							Detail:   responses.ResponseInputImageDetail(detail),
						},
					})
				}
			}
			if len(imageParts) > 0 {
				// Prepend a text label so the model knows these images came from a tool result
				label := responses.ResponseInputContentUnionParam{
					OfInputText: &responses.ResponseInputTextParam{
						Text: "Attached image(s) from tool result:",
					},
				}
				allParts := append([]responses.ResponseInputContentUnionParam{label}, imageParts...)
				input = append(input, responses.ResponseInputItemUnionParam{
					OfInputMessage: &responses.ResponseInputItemMessageParam{
						Role:    "user",
						Content: allParts,
					},
				})
			}
		}
	}
	return input
}

// CreateEmbedding generates an embedding vector for the given text
func (c *Client) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	slog.Debug("Creating OpenAI embedding", "model", c.ModelConfig.Model, "text_length", len(text))

	batchResult, err := c.CreateBatchEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}

	if len(batchResult.Embeddings) == 0 {
		return nil, errors.New("no embedding returned from OpenAI")
	}

	embedding := batchResult.Embeddings[0]

	slog.Debug("OpenAI embedding created successfully",
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

	slog.Debug("Creating OpenAI batch embeddings", "model", c.ModelConfig.Model, "batch_size", len(texts))

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create OpenAI client for batch embedding", "error", err)
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
		slog.Error("OpenAI batch embedding request failed", "error", err)
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

	slog.Debug("OpenAI batch embeddings created successfully",
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
		slog.Debug(startMsg, "model", c.ModelConfig.Model, "num_documents", 0)
		return []float64{}, nil
	}

	slog.Debug(startMsg,
		"model", c.ModelConfig.Model,
		"query_length", len(query),
		"num_documents", len(documents),
		"has_criteria", criteria != "")

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create OpenAI client for reranking", "error", err)
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

	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		slog.Error("OpenAI rerank request failed", "error", err)
		return nil, fmt.Errorf("openai rerank request failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, errors.New("openai rerank response contained no choices")
	}

	raw, err := extractOpenAIContentAsString(resp.Choices[0].Message)
	if err != nil {
		slog.Error("Failed to extract OpenAI rerank content", "error", err)
		return nil, err
	}

	scores, err := parseRerankScores(raw, len(documents))
	if err != nil {
		slog.Error("Failed to parse OpenAI rerank scores", "error", err)
		return nil, err
	}

	slog.Debug("OpenAI reranking complete",
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

// parseRerankScores parses a JSON payload of the form {"scores":[...]} and validates length.
func parseRerankScores(raw string, expected int) ([]float64, error) {
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

// isResponsesModel returns true for OpenAI models that should use the Responses API.
// This includes newer models (gpt-4.1+, o-series, gpt-5) and special variants (-codex).
func isResponsesModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "gpt-4.1") ||
		strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gpt-5") ||
		strings.HasPrefix(m, "codex") ||
		strings.Contains(m, "-codex")
}

func isOpenAIReasoningModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gpt-5")
}

// getOpenAIReasoningEffort resolves the reasoning effort value from the
// model configuration's ThinkingBudget. Returns the effort (minimal|low|medium|high) or an error
func getOpenAIReasoningEffort(cfg *latest.ModelConfig) (effort string, err error) {
	if cfg == nil || cfg.ThinkingBudget == nil {
		return "", nil
	}

	if !isOpenAIReasoningModel(cfg.Model) {
		slog.Warn("OpenAI reasoning effort is not supported for this model, ignoring thinking_budget", "model", cfg.Model)
		return "", nil
	}

	effort = strings.TrimSpace(strings.ToLower(cfg.ThinkingBudget.Effort))
	if effort == "minimal" || effort == "low" || effort == "medium" || effort == "high" {
		return effort, nil
	}

	return "", fmt.Errorf("OpenAI requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: '%s', tokens: '%d'", effort, cfg.ThinkingBudget.Tokens)
}

// jsonSchema is a helper type that implements json.Marshaler for map[string]any
// This allows us to pass schema maps to the OpenAI library which expects json.Marshaler
type jsonSchema map[string]any

func (j jsonSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(j))
}
