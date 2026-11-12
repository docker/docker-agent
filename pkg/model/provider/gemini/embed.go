package gemini

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// outputDimensionalityOpt is the provider_opts key mapped to Gemini's outputDimensionality.
const outputDimensionalityOpt = "output_dimensionality"

// CreateEmbedding generates an embedding vector for the given text.
func (c *Client) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	batch, err := c.CreateBatchEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(batch.Embeddings) == 0 {
		return nil, errors.New("no embedding returned from Gemini")
	}
	return &base.EmbeddingResult{
		Embedding:   batch.Embeddings[0],
		InputTokens: batch.InputTokens,
		TotalTokens: batch.TotalTokens,
		Cost:        batch.Cost,
	}, nil
}

// CreateBatchEmbedding generates embedding vectors for multiple texts, in input order.
func (c *Client) CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error) {
	if len(texts) == 0 {
		return &base.BatchEmbeddingResult{Embeddings: [][]float64{}}, nil
	}

	config, err := c.embedConfig()
	if err != nil {
		return nil, err
	}

	client, err := c.clientFn(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create Gemini client for embedding", "error", err)
		return nil, err
	}

	slog.DebugContext(ctx, "Creating Gemini embeddings", "model", c.ModelConfig.Model, "batch_size", len(texts))

	contents := make([]*genai.Content, len(texts))
	for i, text := range texts {
		contents[i] = genai.NewContentFromText(text, genai.RoleUser)
	}

	var embeddings []*genai.ContentEmbedding
	if c.vertexEmbedsOneContentPerRequest() {
		embeddings, err = c.embedEach(ctx, client, contents, config)
	} else {
		embeddings, err = c.embed(ctx, client, contents, config)
	}
	if err != nil {
		return nil, err
	}

	if len(embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(embeddings))
	}

	result := &base.BatchEmbeddingResult{Embeddings: make([][]float64, len(embeddings))}
	for i, e := range embeddings {
		if e == nil || len(e.Values) == 0 {
			return nil, fmt.Errorf("gemini returned an empty embedding at index %d", i)
		}
		if dims := len(embeddings[0].Values); len(e.Values) != dims {
			return nil, fmt.Errorf("gemini returned inconsistent embedding dimensions: index 0 has %d, index %d has %d", dims, i, len(e.Values))
		}
		vec := make([]float64, len(e.Values))
		for j, v := range e.Values {
			vec[j] = float64(v)
		}
		result.Embeddings[i] = vec
		if e.Statistics != nil {
			result.InputTokens += int64(math.Round(float64(e.Statistics.TokenCount)))
		}
	}
	// Embeddings have no output tokens; Gemini Developer API reports no usage at all.
	result.TotalTokens = result.InputTokens

	slog.DebugContext(ctx, "Gemini embeddings created",
		"batch_size", len(result.Embeddings),
		"dimension", len(result.Embeddings[0]),
		"input_tokens", result.InputTokens)

	return result, nil
}

// embed sends all contents in a single request.
func (c *Client) embed(ctx context.Context, client *genai.Client, contents []*genai.Content, config *genai.EmbedContentConfig) ([]*genai.ContentEmbedding, error) {
	resp, err := client.Models.EmbedContent(ctx, c.ModelConfig.Model, contents, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create embeddings: %w", wrapGeminiError(err))
	}
	if resp == nil {
		return nil, errors.New("gemini embedding response is nil")
	}
	return resp.Embeddings, nil
}

// embedEach sends one request per content. Each response must carry exactly
// one vector, otherwise texts and vectors would silently misalign.
func (c *Client) embedEach(ctx context.Context, client *genai.Client, contents []*genai.Content, config *genai.EmbedContentConfig) ([]*genai.ContentEmbedding, error) {
	embeddings := make([]*genai.ContentEmbedding, len(contents))
	for i, content := range contents {
		batch, err := c.embed(ctx, client, []*genai.Content{content}, config)
		if err != nil {
			return nil, err
		}
		if len(batch) != 1 {
			return nil, fmt.Errorf("expected 1 embedding for input %d, got %d", i, len(batch))
		}
		embeddings[i] = batch[0]
	}
	return embeddings, nil
}

// vertexEmbedsOneContentPerRequest mirrors the genai SDK routing: on Vertex AI,
// Gemini embedding models other than gemini-embedding-001 use embedContent,
// which accepts a single content per request.
func (c *Client) vertexEmbedsOneContentPerRequest() bool {
	model := c.ModelConfig.Model
	return c.apiSurface == apiSurfaceVertexAI && strings.Contains(model, "gemini") && model != "gemini-embedding-001"
}

// embedConfig builds the embedding request config from provider_opts.
// task_type is deliberately not sent: Gemini Embedding 2 rejects it.
func (c *Client) embedConfig() (*genai.EmbedContentConfig, error) {
	config := &genai.EmbedContentConfig{}
	if _, set := c.ModelConfig.ProviderOpts[outputDimensionalityOpt]; !set {
		return config, nil
	}
	dims, ok := providerutil.GetProviderOptInt64(c.ModelConfig.ProviderOpts, outputDimensionalityOpt)
	if !ok || dims <= 0 || dims > math.MaxInt32 {
		return nil, fmt.Errorf("provider_opts.%s must be a positive integer, got %v", outputDimensionalityOpt, c.ModelConfig.ProviderOpts[outputDimensionalityOpt])
	}
	config.OutputDimensionality = new(int32(dims))
	return config, nil
}
