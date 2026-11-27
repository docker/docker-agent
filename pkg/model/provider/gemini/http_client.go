package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/docker/docker-agent/pkg/model/provider/internal/httpx"
)

const (
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	geminiAPIVersion     = "v1beta"
	vertexAPIVersion     = "v1beta1"
)

type apiClientConfig struct {
	APIKey     string
	Project    string
	Location   string
	Backend    Backend
	BaseURL    string
	Headers    http.Header
	HTTPClient *http.Client
}

type apiClient struct {
	apiKey     string
	project    string
	location   string
	backend    Backend
	baseURL    string
	headers    http.Header
	httpClient *http.Client
}

func newAPIClient(cfg apiClientConfig) (*apiClient, error) {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		if cfg.Backend == BackendVertexAI {
			if cfg.Location == "" || cfg.Location == "global" {
				baseURL = "https://aiplatform.googleapis.com"
			} else {
				baseURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com", cfg.Location)
			}
		} else {
			baseURL = defaultGeminiBaseURL
		}
	}

	return &apiClient{
		apiKey:     cfg.APIKey,
		project:    cfg.Project,
		location:   cfg.Location,
		backend:    cfg.Backend,
		baseURL:    baseURL,
		headers:    cfg.Headers.Clone(),
		httpClient: httpClient,
	}, nil
}

func (c *apiClient) modelResourcePath(model string) string {
	model = strings.TrimPrefix(model, "models/")
	if c.backend == BackendVertexAI {
		if c.project != "" && c.location != "" {
			return fmt.Sprintf("/%s/projects/%s/locations/%s/publishers/google/models/%s", vertexAPIVersion, c.project, c.location, model)
		}
		return fmt.Sprintf("/%s/publishers/google/models/%s", vertexAPIVersion, model)
	}
	return fmt.Sprintf("/%s/models/%s", geminiAPIVersion, model)
}

func (c *apiClient) requestHeaders() http.Header {
	h := make(http.Header)
	for k, vals := range c.headers {
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	if c.backend == BackendGeminiAPI && c.apiKey != "" && h.Get("x-goog-api-key") == "" {
		h.Set("x-goog-api-key", c.apiKey)
	}
	return h
}

func buildGenerateContentBody(backend Backend, contents []*Content, cfg *GenerateContentConfig) (*generateContentRequestWire, error) {
	req := &generateContentRequestWire{
		Contents: contents,
	}
	if cfg == nil {
		return req, nil
	}

	if backend == BackendVertexAI && cfg.ToolConfig != nil && cfg.ToolConfig.IncludeServerSideToolInvocations != nil {
		return nil, errors.New("includeServerSideToolInvocations parameter is only supported in Gemini Developer API")
	}

	req.SystemInstruction = cfg.SystemInstruction
	req.Tools = cfg.Tools
	req.ToolConfig = cfg.ToolConfig
	req.ServiceTier = cfg.ServiceTier

	gen := &generationConfigWire{
		Temperature:        cfg.Temperature,
		TopP:               cfg.TopP,
		TopK:               cfg.TopK,
		MaxOutputTokens:    cfg.MaxOutputTokens,
		FrequencyPenalty:   cfg.FrequencyPenalty,
		PresencePenalty:    cfg.PresencePenalty,
		ResponseMIMEType:   cfg.ResponseMIMEType,
		ResponseSchema:     cfg.ResponseSchema,
		ResponseJSONSchema: cfg.ResponseJSONSchema,
		ResponseModalities: cfg.ResponseModalities,
		ThinkingConfig:     cfg.ThinkingConfig,
	}
	if !isEmptyGenerationConfig(gen) {
		req.GenerationConfig = gen
	}
	return req, nil
}

func isEmptyGenerationConfig(g *generationConfigWire) bool {
	return g.Temperature == nil &&
		g.TopP == nil &&
		g.TopK == nil &&
		g.MaxOutputTokens == 0 &&
		g.FrequencyPenalty == nil &&
		g.PresencePenalty == nil &&
		g.ResponseMIMEType == "" &&
		g.ResponseSchema == nil &&
		g.ResponseJSONSchema == nil &&
		len(g.ResponseModalities) == 0 &&
		g.ThinkingConfig == nil
}

func (c *apiClient) generateContent(ctx context.Context, model string, contents []*Content, cfg *GenerateContentConfig) (*GenerateContentResponse, error) {
	body, err := buildGenerateContentBody(c.backend, contents, cfg)
	if err != nil {
		return nil, err
	}
	url := httpx.JoinURL(c.baseURL, c.modelResourcePath(model)+":generateContent")
	var out GenerateContentResponse
	if err := httpx.DoJSON(ctx, c.httpClient, httpx.RequestOptions{
		Method:  http.MethodPost,
		URL:     url,
		Headers: c.requestHeaders(),
	}, body, &out); err != nil {
		return nil, parseGeminiHTTPError(err)
	}
	return &out, nil
}

func (c *apiClient) generateContentStream(ctx context.Context, model string, contents []*Content, cfg *GenerateContentConfig) func(func(*GenerateContentResponse, error) bool) {
	return func(yield func(*GenerateContentResponse, error) bool) {
		body, err := buildGenerateContentBody(c.backend, contents, cfg)
		if err != nil {
			yield(nil, err)
			return
		}
		url := httpx.JoinURL(c.baseURL, c.modelResourcePath(model)+":streamGenerateContent?alt=sse")
		sse, _, err := httpx.DoStream(ctx, c.httpClient, httpx.RequestOptions{ //nolint:bodyclose // closed by sse.Close()
			Method:  http.MethodPost,
			URL:     url,
			Headers: c.requestHeaders(),
		}, body)
		if err != nil {
			yield(nil, parseGeminiHTTPError(err))
			return
		}
		defer sse.Close()

		for sse.Next() {
			ev := sse.Event()
			if ev.Event != "" {
				yield(nil, fmt.Errorf("invalid stream chunk: event: %s", ev.Event))
				return
			}
			data := bytes.TrimSpace(ev.Data)
			if len(data) == 0 {
				continue
			}
			var chunk GenerateContentResponse
			if err := json.Unmarshal(data, &chunk); err != nil {
				yield(nil, fmt.Errorf("invalid stream chunk: %w: %s", err, string(data)))
				return
			}
			if !yield(&chunk, nil) {
				return
			}
		}
		if err := sse.Err(); err != nil {
			yield(nil, err)
		}
	}
}

func (c *apiClient) embedContent(ctx context.Context, model string, contents []*Content, cfg *EmbedContentConfig) ([]*ContentEmbedding, error) {
	if c.backend == BackendVertexAI {
		return c.embedContentVertex(ctx, model, contents, cfg)
	}
	return c.embedContentGeminiAPI(ctx, model, contents, cfg)
}

func (c *apiClient) embedContentGeminiAPI(ctx context.Context, model string, contents []*Content, cfg *EmbedContentConfig) ([]*ContentEmbedding, error) {
	modelName := model
	if !strings.HasPrefix(modelName, "models/") {
		modelName = "models/" + modelName
	}
	type embedReqItem struct {
		Model                string   `json:"model"`
		Content              *Content `json:"content"`
		OutputDimensionality *int32   `json:"outputDimensionality,omitempty"`
	}
	items := make([]embedReqItem, len(contents))
	for i, content := range contents {
		item := embedReqItem{
			Model:   modelName,
			Content: content,
		}
		if cfg != nil && cfg.OutputDimensionality != nil {
			item.OutputDimensionality = cfg.OutputDimensionality
		}
		items[i] = item
	}

	url := httpx.JoinURL(c.baseURL, c.modelResourcePath(model)+":batchEmbedContents")
	var resp struct {
		Embeddings []*ContentEmbedding `json:"embeddings"`
	}
	if err := httpx.DoJSON(ctx, c.httpClient, httpx.RequestOptions{
		Method:  http.MethodPost,
		URL:     url,
		Headers: c.requestHeaders(),
	}, map[string]any{"requests": items}, &resp); err != nil {
		return nil, parseGeminiHTTPError(err)
	}
	return resp.Embeddings, nil
}

func (c *apiClient) embedContentVertex(ctx context.Context, model string, contents []*Content, cfg *EmbedContentConfig) ([]*ContentEmbedding, error) {
	// Gemini Embedding 2+ on Vertex uses :embedContent; gemini-embedding-001 and text-embedding-* use :predict.
	if strings.Contains(model, "gemini") && model != "gemini-embedding-001" {
		if len(contents) != 1 {
			return nil, fmt.Errorf("vertex embedContent expects 1 content, got %d", len(contents))
		}
		body := map[string]any{
			"content": contents[0],
		}
		if cfg != nil && cfg.OutputDimensionality != nil {
			body["embedContentConfig"] = map[string]any{
				"outputDimensionality": *cfg.OutputDimensionality,
			}
		}
		url := httpx.JoinURL(c.baseURL, c.modelResourcePath(model)+":embedContent")
		var resp struct {
			Embedding     *ContentEmbedding `json:"embedding"`
			UsageMetadata *struct {
				PromptTokenCount float32 `json:"promptTokenCount"`
			} `json:"usageMetadata"`
			Truncated bool `json:"truncated"`
		}
		if err := httpx.DoJSON(ctx, c.httpClient, httpx.RequestOptions{
			Method:  http.MethodPost,
			URL:     url,
			Headers: c.requestHeaders(),
		}, body, &resp); err != nil {
			return nil, parseGeminiHTTPError(err)
		}
		if resp.Embedding == nil {
			return nil, nil
		}
		if resp.UsageMetadata != nil {
			resp.Embedding.Statistics = &ContentEmbeddingStatistics{
				TokenCount: resp.UsageMetadata.PromptTokenCount,
				Truncated:  resp.Truncated,
			}
		}
		return []*ContentEmbedding{resp.Embedding}, nil
	}

	// Vertex :predict endpoint
	instances := make([]map[string]any, len(contents))
	for i, content := range contents {
		var text strings.Builder
		if content != nil {
			for _, p := range content.Parts {
				if p != nil {
					text.WriteString(p.Text)
				}
			}
		}
		instances[i] = map[string]any{"content": text.String()}
	}
	body := map[string]any{
		"instances": instances,
	}
	if cfg != nil && cfg.OutputDimensionality != nil {
		body["parameters"] = map[string]any{
			"outputDimensionality": *cfg.OutputDimensionality,
		}
	}
	url := httpx.JoinURL(c.baseURL, c.modelResourcePath(model)+":predict")
	var resp struct {
		Predictions []struct {
			Embeddings *ContentEmbedding `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := httpx.DoJSON(ctx, c.httpClient, httpx.RequestOptions{
		Method:  http.MethodPost,
		URL:     url,
		Headers: c.requestHeaders(),
	}, body, &resp); err != nil {
		return nil, parseGeminiHTTPError(err)
	}
	out := make([]*ContentEmbedding, len(resp.Predictions))
	for i, p := range resp.Predictions {
		out[i] = p.Embeddings
	}
	return out, nil
}

// parseGeminiHTTPError converts an *httpx.APIError into a Gemini APIError value
// so wrapGeminiError can classify and wrap it identically to google.golang.org/genai.
func parseGeminiHTTPError(err error) error {
	apiErr, ok := httpx.IsAPIError(err)
	if !ok {
		return err
	}
	parsed := APIError{
		Code:   apiErr.StatusCode,
		Status: http.StatusText(apiErr.StatusCode),
	}

	body := bytes.TrimSpace(apiErr.Body)
	var envelope struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil {
		parsed = *envelope.Error
		if parsed.Code == 0 {
			parsed.Code = apiErr.StatusCode
		}
		return parsed
	}
	// Streaming endpoints sometimes wrap errors in a JSON array: [{"error": {...}}]
	var arr []struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 && arr[0].Error != nil {
		parsed = *arr[0].Error
		if parsed.Code == 0 {
			parsed.Code = apiErr.StatusCode
		}
		return parsed
	}

	parsed.Message = string(body)
	return parsed
}
