package gemini

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/prompts"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client represents a Gemini client wrapper
// It implements the provider.Provider interface
type Client struct {
	base.Config
	clientFn func(context.Context) (*genai.Client, error)
}

// NewClient creates a new Gemini client from the provided configuration
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}

	if cfg.Provider != "google" {
		return nil, errors.New("model type must be 'google'")
	}

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	var clientFn func(context.Context) (*genai.Client, error)
	if gateway := globalOptions.Gateway(); gateway == "" {
		var (
			httpClient *http.Client
			backend    genai.Backend
			apiKey     string
			project    string
			location   string
		)
		// project/location take priority over API key, like in the genai client.
		if cfg.ProviderOpts["project"] != nil || cfg.ProviderOpts["location"] != nil {
			var err error

			project, err = environment.Expand(ctx, providerOption(cfg, "project"), env)
			if err != nil {
				return nil, fmt.Errorf("expanding project: %w", err)
			}
			if project == "" {
				return nil, errors.New("project must be set")
			}

			location, err = environment.Expand(ctx, providerOption(cfg, "location"), env)
			if err != nil {
				return nil, fmt.Errorf("expanding location: %w", err)
			}
			if location == "" {
				return nil, errors.New("location must be set")
			}

			backend = genai.BackendVertexAI
			httpClient = nil // Use default client
		} else if _, exist := env.Get(ctx, "GOOGLE_GENAI_USE_VERTEXAI"); exist {
			project, _ = env.Get(ctx, "GOOGLE_CLOUD_PROJECT")
			location, _ = env.Get(ctx, "GOOGLE_CLOUD_LOCATION")
			backend = genai.BackendVertexAI
			httpClient = nil // Use default client
		} else {
			if value, exist := env.Get(ctx, "GEMINI_API_KEY"); exist {
				apiKey = value
			}
			if value, exist := env.Get(ctx, "GOOGLE_API_KEY"); exist {
				apiKey = value
			}
			if apiKey == "" {
				return nil, errors.New("GOOGLE_API_KEY or GEMINI_API_KEY environment variable is required")
			}

			backend = genai.BackendGeminiAPI
			httpClient = httpclient.NewHTTPClient()
		}

		// Expand environment variables in base URL (e.g., ${VAR_NAME})
		baseURL := cfg.BaseURL
		if baseURL != "" {
			expanded, err := environment.Expand(ctx, baseURL, env)
			if err != nil {
				return nil, fmt.Errorf("expanding base_url: %w", err)
			}
			baseURL = expanded
		}

		// Build custom headers from provider config
		httpHeaders := make(http.Header)
		if cfg.ProviderOpts != nil {
			if headers, exists := cfg.ProviderOpts["headers"]; exists {
				headersMap := make(map[string]string)

				switch h := headers.(type) {
				case map[string]string:
					headersMap = h
				case map[interface{}]interface{}:
					for k, v := range h {
						keyStr, okKey := k.(string)
						valStr, okVal := v.(string)
						if !okKey || !okVal {
							return nil, fmt.Errorf("invalid header key/value type: key=%T, value=%T", k, v)
						}
						headersMap[keyStr] = valStr
					}
				default:
					return nil, fmt.Errorf("invalid headers configuration: expected map[string]string, got %T", headers)
				}

				for key, value := range headersMap {
					expandedValue, err := environment.Expand(ctx, value, env)
					if err != nil {
						return nil, fmt.Errorf("expanding header %q: %w", key, err)
					}
					httpHeaders.Set(key, expandedValue)
					slog.Debug("Applied custom header", "header", key, "provider", cfg.Provider)
				}
				slog.Debug("Applying custom headers", "count", len(headersMap), "provider", cfg.Provider)
			}
		}

		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:     apiKey,
			Project:    project,
			Location:   location,
			Backend:    backend,
			HTTPClient: httpClient,
			HTTPOptions: genai.HTTPOptions{
				BaseURL: baseURL,
				Headers: httpHeaders,
			},
		})
		if err != nil {
			return nil, err
		}

		clientFn = func(context.Context) (*genai.Client, error) {
			return client, nil
		}
	} else {
		// Fail fast if Docker Desktop's auth token isn't available
		if token, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); token == "" {
			slog.Error("Gemini client creation failed", "error", "failed to get Docker Desktop's authentication token")
			return nil, errors.New("sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
		}

		// When using a Gateway, tokens are short-lived.
		clientFn = func(ctx context.Context) (*genai.Client, error) {
			// Query a fresh auth token each time the client is used
			authToken, _ := env.Get(ctx, environment.DockerDesktopTokenEnv)
			if authToken == "" {
				return nil, errors.New("failed to get Docker Desktop token for Gateway")
			}

			url, err := url.Parse(gateway)
			if err != nil {
				return nil, fmt.Errorf("invalid gateway URL: %w", err)
			}
			baseURL := fmt.Sprintf("%s://%s%s/", url.Scheme, url.Host, url.Path)

			httpOptions := []httpclient.Opt{
				httpclient.WithProxiedBaseURL(cmp.Or(cfg.BaseURL, "https://generativelanguage.googleapis.com/")),
				httpclient.WithProvider(cfg.Provider),
				httpclient.WithModel(cfg.Model),
				httpclient.WithModelName(cfg.Name),
				httpclient.WithQuery(url.Query()),
			}
			if globalOptions.GeneratingTitle() {
				httpOptions = append(httpOptions, httpclient.WithHeader("X-Cagent-GeneratingTitle", "1"))
			}

			return genai.NewClient(ctx, &genai.ClientConfig{
				APIKey:     authToken,
				Backend:    genai.BackendGeminiAPI,
				HTTPClient: httpclient.NewHTTPClient(httpOptions...),
				HTTPOptions: genai.HTTPOptions{
					BaseURL: baseURL,
					Headers: http.Header{
						"Authorization": []string{"Bearer " + authToken},
					},
				},
			})
		}
	}

	slog.Debug("Gemini client created successfully", "model", cfg.Model)

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: clientFn,
	}, nil
}

// convertMessagesToGemini converts chat.Messages into Gemini Contents
func convertMessagesToGemini(messages []chat.Message) []*genai.Content {
	contents := make([]*genai.Content, 0, len(messages))
	for i := range messages {
		msg := &messages[i]

		// Skip empty messages
		if msg.Content == "" && len(msg.MultiContent) == 0 && len(msg.ToolCalls) == 0 && msg.ToolCallID == "" {
			continue
		}

		role := messageRoleToGemini(msg.Role)

		// Handle tool responses
		if msg.Role == chat.MessageRoleTool && msg.ToolCallID != "" {
			response := map[string]any{"result": msg.Content}

			// Check for image content in MultiContent
			var imageParts []*genai.FunctionResponsePart
			for _, mc := range msg.MultiContent {
				if mc.Type == chat.MessagePartTypeImageURL && mc.ImageURL != nil && strings.HasPrefix(mc.ImageURL.URL, "data:") {
					urlParts := strings.SplitN(mc.ImageURL.URL, ",", 2)
					if len(urlParts) == 2 {
						mimeType := extractMimeType(urlParts[0])
						data, err := base64.StdEncoding.DecodeString(urlParts[1])
						if err == nil {
							imageParts = append(imageParts, genai.NewFunctionResponsePartFromBytes(data, mimeType))
						}
					}
				}
			}

			var part *genai.Part
			if len(imageParts) > 0 {
				part = genai.NewPartFromFunctionResponseWithParts(msg.ToolCallID, response, imageParts)
			} else {
				part = genai.NewPartFromFunctionResponse(msg.ToolCallID, response)
			}
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{part}, role))
			continue
		}

		// Handle assistant messages with tool calls
		if msg.Role == chat.MessageRoleAssistant && len(msg.ToolCalls) > 0 {
			parts := make([]*genai.Part, 0, len(msg.ToolCalls)+1)

			// Add text content if present
			if msg.Content != "" {
				parts = append(parts, newTextPartWithSignature(msg.Content, msg.ThoughtSignature))
			}

			// Add function calls
			for _, tc := range msg.ToolCalls {
				var args map[string]any
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				}
				fc := genai.NewPartFromFunctionCall(tc.Function.Name, args)
				if len(msg.ThoughtSignature) > 0 {
					fc.ThoughtSignature = msg.ThoughtSignature
				}
				parts = append(parts, fc)
			}

			contents = append(contents, genai.NewContentFromParts(parts, role))
			continue
		}

		// Handle regular messages
		if len(msg.MultiContent) > 0 {
			parts := convertMultiContent(msg.MultiContent, msg.ThoughtSignature)
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, role))
			}
		} else if msg.Content != "" {
			part := newTextPartWithSignature(msg.Content, msg.ThoughtSignature)
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{part}, role))
		}
	}
	return contents
}

// messageRoleToGemini converts chat.MessageRole to genai.Role
func messageRoleToGemini(role chat.MessageRole) genai.Role {
	if role == chat.MessageRoleAssistant {
		return genai.RoleModel
	}
	return genai.RoleUser
}

// newTextPartWithSignature creates a text part with optional thought signature
func newTextPartWithSignature(text string, signature []byte) *genai.Part {
	part := genai.NewPartFromText(text)
	if len(signature) > 0 {
		part.ThoughtSignature = signature
	}
	return part
}

// convertMultiContent converts multi-part content to Gemini parts
func convertMultiContent(multiContent []chat.MessagePart, thoughtSignature []byte) []*genai.Part {
	parts := make([]*genai.Part, 0, len(multiContent))
	for _, part := range multiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			parts = append(parts, newTextPartWithSignature(part.Text, thoughtSignature))
		case chat.MessagePartTypeImageURL:
			if imgPart := convertImageURLToPart(part.ImageURL); imgPart != nil {
				parts = append(parts, imgPart)
			}
		}
	}
	return parts
}

// convertImageURLToPart converts an image URL to a Gemini Part
// Supports data URLs with base64-encoded image data
func convertImageURLToPart(imageURL *chat.MessageImageURL) *genai.Part {
	if imageURL == nil || !strings.HasPrefix(imageURL.URL, "data:") {
		return nil
	}

	// Parse data URL format: data:[<mediatype>][;base64],<data>
	urlParts := strings.SplitN(imageURL.URL, ",", 2)
	if len(urlParts) != 2 {
		return nil
	}

	imageData, err := base64.StdEncoding.DecodeString(urlParts[1])
	if err != nil {
		return nil
	}

	mimeType := extractMimeType(urlParts[0])
	return genai.NewPartFromBytes(imageData, mimeType)
}

// extractMimeType extracts the MIME type from a data URL prefix
func extractMimeType(dataURLPrefix string) string {
	for _, mimeType := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		if strings.Contains(dataURLPrefix, mimeType) {
			return mimeType
		}
	}
	return "image/jpeg" // Default fallback
}

// buildConfig creates GenerateContentConfig from model config
func (c *Client) buildConfig() *genai.GenerateContentConfig {
	config := &genai.GenerateContentConfig{}
	if c.ModelConfig.MaxTokens != nil {
		config.MaxOutputTokens = int32(*c.ModelConfig.MaxTokens)
	}
	if c.ModelConfig.Temperature != nil {
		config.Temperature = new(float32(*c.ModelConfig.Temperature))
	}
	if c.ModelConfig.TopP != nil {
		config.TopP = new(float32(*c.ModelConfig.TopP))
	}
	if c.ModelConfig.FrequencyPenalty != nil {
		config.FrequencyPenalty = new(float32(*c.ModelConfig.FrequencyPenalty))
	}
	if c.ModelConfig.PresencePenalty != nil {
		config.PresencePenalty = new(float32(*c.ModelConfig.PresencePenalty))
	}

	// Apply thinking configuration for Gemini models.
	// Per official docs: https://ai.google.dev/gemini-api/docs/thinking
	//
	// Gemini 2.5 models use token-based configuration (thinkingBudget):
	// - Set thinkingBudget to 0 to disable thinking
	// - Set thinkingBudget to -1 for dynamic thinking (model decides)
	// - Set to a specific value for a fixed token budget
	//   (max 24576 for most models, 32768 for Gemini 2.5 Pro)
	//
	// Gemini 3 models use level-based configuration (thinkingLevel):
	// - Gemini 3 Pro: "low", "high"
	// - Gemini 3 Flash: "minimal", "low", "medium", "high"
	//
	// When thinking is explicitly disabled via ModelOptions (e.g., for title generation),
	// we set ThinkingBudget to 0 to disable thinking completely. This is required for
	// operations where max_tokens is very low and thinking would cause the request to
	// hang or fail. IncludeThoughts=false is also set to ensure no thinking content
	// is returned.
	if thinking := c.ModelOptions.Thinking(); thinking != nil && !*thinking {
		model := strings.ToLower(c.ModelConfig.Model)
		if isGemini3PlusModel(model) {
			// Gemini 3 models require thinking — they reject ThinkingBudget=0.
			// Use the lowest level instead and bump MaxOutputTokens so that
			// even a tiny caller budget (e.g. 20 for title generation) leaves
			// room for the model's internal reasoning.
			config.ThinkingConfig = &genai.ThinkingConfig{
				IncludeThoughts: false,
				ThinkingLevel:   genai.ThinkingLevelLow,
			}
			const minOutputTokens int32 = 200
			if config.MaxOutputTokens < minOutputTokens {
				config.MaxOutputTokens = minOutputTokens
			}
			slog.Debug("Gemini 3 thinking reduced to low (cannot be fully disabled)",
				"model", c.ModelConfig.Model,
				"max_output_tokens", config.MaxOutputTokens,
			)
		} else {
			// Gemini 2.5 and older: ThinkingBudget=0 disables thinking.
			config.ThinkingConfig = &genai.ThinkingConfig{
				IncludeThoughts: false,
				ThinkingBudget:  new(int32(0)),
			}
			slog.Debug("Gemini thinking explicitly disabled via ModelOptions",
				"model", c.ModelConfig.Model,
				"max_output_tokens", config.MaxOutputTokens,
			)
		}
	} else if c.ModelConfig.ThinkingBudget != nil {
		c.applyThinkingConfig(config)
	} else {
		slog.Debug("Gemini buildConfig: no thinking configuration applied",
			"model", c.ModelConfig.Model,
			"thinking_option", c.ModelOptions.Thinking(),
			"thinking_budget", c.ModelConfig.ThinkingBudget,
		)
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		config.ResponseMIMEType = "application/json"
		config.ResponseJsonSchema = structuredOutput.Schema
	}

	return config
}

// applyThinkingConfig applies the appropriate thinking configuration based on model type.
func (c *Client) applyThinkingConfig(config *genai.GenerateContentConfig) {
	if config.ThinkingConfig == nil {
		config.ThinkingConfig = &genai.ThinkingConfig{}
	}
	config.ThinkingConfig.IncludeThoughts = true

	model := strings.ToLower(c.ModelConfig.Model)

	// Gemini 3+ models use ThinkingLevel (effort-based)
	if isGemini3PlusModel(model) {
		c.applyGemini3ThinkingLevel(config)
		return
	}

	// Gemini 2.5 and other models use ThinkingBudget (token-based)
	c.applyGemini25ThinkingBudget(config)
}

// applyGemini3ThinkingLevel applies level-based thinking for Gemini 3 models.
func (c *Client) applyGemini3ThinkingLevel(config *genai.GenerateContentConfig) {
	effort := strings.ToLower(c.ModelConfig.ThinkingBudget.Effort)

	var level genai.ThinkingLevel
	switch effort {
	case "minimal":
		level = genai.ThinkingLevelMinimal
	case "low":
		level = genai.ThinkingLevelLow
	case "medium":
		level = genai.ThinkingLevelMedium
	case "high":
		level = genai.ThinkingLevelHigh
	default:
		// If effort is not set but tokens are, fall back to token-based config
		if c.ModelConfig.ThinkingBudget.Tokens != 0 {
			slog.Warn("Gemini 3 models use thinkingLevel, not thinkingBudget tokens; falling back to token-based config",
				"model", c.ModelConfig.Model,
				"tokens", c.ModelConfig.ThinkingBudget.Tokens,
			)
			c.applyGemini25ThinkingBudget(config)
			return
		}
		// Default to high if no valid effort specified
		level = genai.ThinkingLevelHigh
		slog.Debug("Gemini 3 using default thinking level",
			"model", c.ModelConfig.Model,
			"level", "high",
		)
		config.ThinkingConfig.ThinkingLevel = level
		return
	}

	config.ThinkingConfig.ThinkingLevel = level
	slog.Debug("Gemini 3 request using thinkingLevel",
		"model", c.ModelConfig.Model,
		"level", effort,
	)
}

// applyGemini25ThinkingBudget applies token-based thinking for Gemini 2.5 and other models.
func (c *Client) applyGemini25ThinkingBudget(config *genai.GenerateContentConfig) {
	tokens := c.ModelConfig.ThinkingBudget.Tokens
	config.ThinkingConfig.ThinkingBudget = new(int32(tokens))

	switch tokens {
	case 0:
		slog.Debug("Gemini request with thinking disabled", "budget_tokens", tokens)
	case -1:
		slog.Debug("Gemini request with dynamic thinking", "budget_tokens", tokens)
	default:
		slog.Debug("Gemini request using thinking_budget", "budget_tokens", tokens)
	}
}

// convertToolsToGemini converts tools to Gemini format
func convertToolsToGemini(requestTools []tools.Tool) ([]*genai.Tool, error) {
	if len(requestTools) == 0 {
		return nil, nil
	}

	funcs := make([]*genai.FunctionDeclaration, 0, len(requestTools))
	for _, tool := range requestTools {
		parameters, err := ConvertParametersToSchema(tool.Parameters)
		if err != nil {
			return nil, err
		}

		funcs = append(funcs, &genai.FunctionDeclaration{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		})
	}

	return []*genai.Tool{{
		FunctionDeclarations: funcs,
	}}, nil
}

// ConvertParametersToSchema converts parameters to Gemini Schema format
func ConvertParametersToSchema(params any) (*genai.Schema, error) {
	m, err := tools.SchemaToMap(params)
	if err != nil {
		return nil, err
	}

	normalizeTypeFields(m)

	var schema *genai.Schema
	if err := tools.ConvertSchema(m, &schema); err != nil {
		return nil, err
	}

	return schema, nil
}

// normalizeTypeFields recursively converts type arrays to single string values.
// JSON Schema allows "type": ["string", "null"] but Gemini expects a single type.
// This picks the first non-null type from arrays.
func normalizeTypeFields(m map[string]any) {
	if typeVal, ok := m["type"]; ok {
		if typeArr, isArray := typeVal.([]any); isArray {
			m["type"] = pickNonNullType(typeArr)
		}
	}

	if props, ok := m["properties"].(map[string]any); ok {
		for _, prop := range props {
			if propMap, ok := prop.(map[string]any); ok {
				normalizeTypeFields(propMap)
			}
		}
	}

	if items, ok := m["items"].(map[string]any); ok {
		normalizeTypeFields(items)
	}
}

func pickNonNullType(typeArr []any) string {
	for _, t := range typeArr {
		if s, ok := t.(string); ok && s != "null" {
			return s
		}
	}
	if len(typeArr) > 0 {
		if s, ok := typeArr[0].(string); ok {
			return s
		}
	}
	return "string"
}

// CreateChatCompletionStream creates a streaming chat completion request
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	if len(messages) == 0 {
		return nil, errors.New("at least one message is required")
	}

	config := c.buildConfig()

	// Add tools to config if provided
	if len(requestTools) > 0 {
		allTools, err := convertToolsToGemini(requestTools)
		if err != nil {
			slog.Error("Failed to convert tools to Gemini format", "error", err)
			return nil, err
		}

		config.Tools = allTools

		// Enable function calling
		config.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAuto,
			},
		}

		// Debug: Log the tools we're sending
		slog.Debug("Gemini tools config", "tools", config.Tools)
		for _, tool := range config.Tools {
			for _, fn := range tool.FunctionDeclarations {
				slog.Debug("Function", "name", fn.Name, "desc", fn.Description, "params", fn.Parameters)
			}
		}
	}

	contents := convertMessagesToGemini(messages)

	// Debug: Log the messages we're sending
	slog.Debug("Gemini messages", "count", len(contents))
	for i, content := range contents {
		slog.Debug("Message", "index", i, "role", content.Role)
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create Gemini client", "error", err)
		return nil, err
	}

	// Build a fresh client per request when using the gateway
	iter := client.Models.GenerateContentStream(ctx, c.ModelConfig.Model, contents, config)
	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
	return NewStreamAdapter(iter, c.ModelConfig.Model, trackUsage), nil
}

// Rerank scores documents by relevance to the query using Gemini's structured
// output feature. It returns relevance scores in the same order as input documents.
func (c *Client) Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error) {
	const logPrefix = "Gemini reranking request"

	if len(documents) == 0 {
		slog.Debug(logPrefix, "model", c.ModelConfig.Model, "num_documents", 0)
		return []float64{}, nil
	}

	slog.Debug(logPrefix,
		"model", c.ModelConfig.Model,
		"query_length", len(query),
		"num_documents", len(documents),
		"has_criteria", criteria != "")

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.Error("Failed to create Gemini client for reranking", "error", err)
		return nil, err
	}

	// Build user prompt with query and numbered documents (including metadata)
	userPrompt := prompts.BuildRerankDocumentsPrompt(query, documents)

	// Build system prompt with Gemini-specific JSON format instructions
	jsonFormatInstruction := `Return a JSON object with a "scores" array containing one number per document, in order.`
	systemPrompt := prompts.BuildRerankSystemPrompt(documents, criteria, c.ModelConfig.ProviderOpts, jsonFormatInstruction)

	// Create a single user turn that includes both system-like instruction and data.
	content := genai.NewContentFromParts(
		[]*genai.Part{
			genai.NewPartFromText(systemPrompt),
			genai.NewPartFromText(userPrompt),
		},
		genai.RoleUser,
	)

	// Use Gemini's structured output feature to enforce the JSON schema.
	// This eliminates the need for fallback parsing strategies.
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
		"required": []string{"scores"},
	}

	// Start with standard config from model definition (respects max_tokens,
	// temperature, top_p, etc. from the model config).
	// If the user hasn't configured these, we rely on Gemini API defaults.
	cfg := c.buildConfig()

	// Override with reranking-specific structured output schema.
	cfg.ResponseMIMEType = "application/json"
	cfg.ResponseJsonSchema = schema

	// For reranking, default temperature to 0 for deterministic scoring if not explicitly set.
	if c.ModelConfig.Temperature == nil {
		cfg.Temperature = new(float32(0.0))
	}

	// Disable thinking for reranking - we want quick, deterministic scoring
	// without wasting tokens on internal reasoning. This overrides any
	// thinking_budget from the model config for this specific use case.
	cfg.ThinkingConfig = &genai.ThinkingConfig{
		IncludeThoughts: false,
	}

	resp, err := client.Models.GenerateContent(ctx, c.ModelConfig.Model, []*genai.Content{content}, cfg)
	if err != nil {
		slog.Error("Gemini rerank request failed", "error", err)
		return nil, fmt.Errorf("gemini rerank request failed: %w", err)
	}

	// Check if the request was blocked by safety filters
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini blocked request: %v", resp.PromptFeedback.BlockReason)
	}

	rawJSON, err := extractGeminiStructuredJSON(resp)
	if err != nil {
		slog.Error("Failed to extract Gemini structured JSON", "error", err)
		return nil, err
	}

	scores, err := parseRerankScoresStrict(rawJSON, len(documents))
	if err != nil {
		slog.Error("Failed to parse Gemini rerank scores", "error", err)
		return nil, err
	}

	slog.Debug("Gemini reranking complete",
		"model", c.ModelConfig.Model,
		"num_scores", len(scores))

	return scores, nil
}

// extractGeminiStructuredJSON extracts the JSON string from a
// GenerateContentResponse with structured output enabled.
func extractGeminiStructuredJSON(resp *genai.GenerateContentResponse) (string, error) {
	if resp == nil {
		return "", errors.New("gemini response is nil")
	}

	if len(resp.Candidates) == 0 {
		return "", errors.New("gemini response has no candidates")
	}

	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}

		for _, part := range cand.Content.Parts {
			if part != nil && part.Text != "" {
				return part.Text, nil
			}
		}
	}

	return "", errors.New("no text part found in Gemini response for structured JSON")
}

// parseRerankScoresStrict parses a JSON payload of the form {"scores":[...]}
// and validates length. This version does NOT have fallback parsing since
// structured outputs guarantee valid JSON.
func parseRerankScoresStrict(raw string, expected int) ([]float64, error) {
	type rerankResponse struct {
		Scores []float64 `json:"scores"`
	}

	var rr rerankResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &rr); err != nil {
		return nil, fmt.Errorf("failed to parse rerank JSON: %w", err)
	}

	if len(rr.Scores) != expected {
		return nil, fmt.Errorf("expected %d scores, got %d", expected, len(rr.Scores))
	}

	return rr.Scores, nil
}

func providerOption(cfg *latest.ModelConfig, name string) string {
	v := cfg.ProviderOpts[name]
	if v, ok := v.(string); ok {
		return v
	}
	return ""
}

// isGemini3PlusModel returns true if the lowercased model name is a Gemini 3+
// model. It matches both "gemini-3-<family>" and "gemini-3.X-<family>" patterns.
// NOTE: keep in sync with gemini3Family in pkg/model/provider/provider.go.
func isGemini3PlusModel(model string) bool {
	if !strings.HasPrefix(model, "gemini-3") {
		return false
	}
	rest := model[len("gemini-3"):]
	// "gemini-3-pro" → rest = "-pro"
	// "gemini-3.1-flash" → rest = ".1-flash"
	return rest != "" && (rest[0] == '-' || (rest[0] == '.' && strings.Contains(rest, "-")))
}
