package dmr

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/rag/types"
)

// CreateEmbedding generates an embedding vector for the given text with usage tracking.
func (c *Client) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	batch, err := c.CreateBatchEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(batch.Embeddings) == 0 {
		return nil, errors.New("no embedding returned from DMR")
	}
	return &base.EmbeddingResult{
		Embedding:   batch.Embeddings[0],
		InputTokens: batch.InputTokens,
		TotalTokens: batch.TotalTokens,
		Cost:        batch.Cost,
	}, nil
}

// CreateBatchEmbedding generates embedding vectors for multiple texts with usage tracking.
func (c *Client) CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error) {
	if len(texts) == 0 {
		return &base.BatchEmbeddingResult{Embeddings: [][]float64{}}, nil
	}

	slog.DebugContext(ctx, "Creating DMR embeddings", "model", c.ModelConfig.Model, "batch_size", len(texts), "base_url", c.BaseURL)

	response, err := c.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
		Model: c.ModelConfig.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create embeddings: %w", err)
	}

	if len(response.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(response.Data))
	}

	embeddings := make([][]float64, len(response.Data))
	for i, data := range response.Data {
		vec := make([]float64, len(data.Embedding))
		copy(vec, data.Embedding)
		embeddings[i] = vec
	}

	slog.DebugContext(ctx, "DMR embeddings created",
		"batch_size", len(embeddings),
		"dimension", len(embeddings[0]),
		"input_tokens", response.Usage.PromptTokens,
		"total_tokens", response.Usage.TotalTokens)

	return &base.BatchEmbeddingResult{
		Embeddings:  embeddings,
		InputTokens: response.Usage.PromptTokens,
		TotalTokens: response.Usage.TotalTokens,
		Cost:        0, // DMR is local/free
	}, nil
}

// rerankRequest is the JSON body sent to the DMR /rerank endpoint.
type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

// rerankResponse is the JSON body returned by the DMR /rerank endpoint.
type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// rerankBaseURL extracts the scheme://host portion of the OpenAI base URL.
// The /rerank endpoint lives at the host root, not under /engines/v1/.
func rerankBaseURL(openaiBaseURL string) (string, error) {
	u, err := url.Parse(openaiBaseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	scheme := cmp.Or(u.Scheme, "http")
	host := cmp.Or(u.Host, "127.0.0.1:12434")
	return fmt.Sprintf("%s://%s", scheme, host), nil
}

// Rerank scores documents by relevance to the query using a reranking model.
// Returns relevance scores in the same order as input documents.
func (c *Client) Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error) {
	if len(documents) == 0 {
		return []float64{}, nil
	}

	startTime := time.Now()

	if criteria != "" {
		slog.WarnContext(ctx, "DMR reranking does not support custom criteria", "model", c.ModelConfig.Model)
	}

	documentStrings := make([]string, len(documents))
	for i, doc := range documents {
		documentStrings[i] = doc.Content
	}

	baseURL, err := rerankBaseURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	rerankURL := baseURL + "/rerank"

	reqData, err := json.Marshal(rerankRequest{
		Model:     c.ModelConfig.Model,
		Query:     query,
		Documents: documentStrings,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal rerank request: %w", err)
	}

	slog.DebugContext(ctx, "DMR reranking", "model", c.ModelConfig.Model, "url", rerankURL, "num_documents", len(documents))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rerankURL, bytes.NewReader(reqData))
	if err != nil {
		return nil, fmt.Errorf("failed to create rerank request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("rerank request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var rerankResp rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&rerankResp); err != nil {
		return nil, fmt.Errorf("failed to decode rerank response: %w", err)
	}

	if len(rerankResp.Results) != len(documents) {
		return nil, fmt.Errorf("expected %d rerank scores, got %d", len(documents), len(rerankResp.Results))
	}

	// Map results back to input order and apply sigmoid normalization.
	// Sigmoid preserves absolute magnitude: positive logits → >0.5, negative → <0.5.
	scores := make([]float64, len(documents))
	for _, result := range rerankResp.Results {
		if result.Index < 0 || result.Index >= len(documents) {
			return nil, fmt.Errorf("invalid result index %d", result.Index)
		}
		scores[result.Index] = sigmoid(result.RelevanceScore)
	}

	slog.DebugContext(ctx, "DMR reranking complete",
		"model", c.ModelConfig.Model,
		"num_scores", len(scores),
		"prompt_tokens", rerankResp.Usage.PromptTokens,
		"total_tokens", rerankResp.Usage.TotalTokens,
		"duration_ms", time.Since(startTime).Milliseconds())

	return scores, nil
}

// sigmoid applies the sigmoid function to normalize a raw logit score to [0, 1].
// Formula: 1 / (1 + exp(-x))
func sigmoid(x float64) float64 {
	return 1.0 / (1.0 + math.Exp(-x))
}
