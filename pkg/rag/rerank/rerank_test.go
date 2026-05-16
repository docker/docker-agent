package rerank

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// fakeRerankingProvider implements provider.RerankingProvider for testing.
type fakeRerankingProvider struct {
	base.Config

	scores []float64
	err    error
}

func (f *fakeRerankingProvider) ID() modelsdev.ID {
	return modelsdev.NewID("test", "fake-reranker")
}

func (f *fakeRerankingProvider) CreateChatCompletionStream(
	_ context.Context,
	_ []chat.Message,
	_ []tools.Tool,
) (chat.MessageStream, error) {
	// Not used in these tests.
	return nil, nil
}

func (f *fakeRerankingProvider) BaseConfig() base.Config {
	return f.Config
}

func (f *fakeRerankingProvider) Rerank(
	_ context.Context,
	_ string,
	documents []types.Document,
	_ string, // criteria (not used in test)
) ([]float64, error) {
	if f.err != nil {
		return nil, f.err
	}

	// Return as many scores as requested documents, falling back to zeros if needed.
	out := make([]float64, len(documents))
	for i := range documents {
		if i < len(f.scores) {
			out[i] = f.scores[i]
		}
	}
	return out, nil
}

// fakeProviderWithoutRerank implements provider.Provider but not RerankingProvider
// to verify error handling when the model does not support reranking.
type fakeProviderWithoutRerank struct {
	base.Config
}

func (f *fakeProviderWithoutRerank) ID() modelsdev.ID {
	return modelsdev.NewID("test", "fake-no-rerank")
}

func (f *fakeProviderWithoutRerank) CreateChatCompletionStream(
	_ context.Context,
	_ []chat.Message,
	_ []tools.Tool,
) (chat.MessageStream, error) {
	return nil, nil
}

func (f *fakeProviderWithoutRerank) BaseConfig() base.Config {
	return f.Config
}

var (
	_ provider.Provider          = (*fakeRerankingProvider)(nil)
	_ provider.RerankingProvider = (*fakeRerankingProvider)(nil)
	_ provider.Provider          = (*fakeProviderWithoutRerank)(nil)
)

func TestLLMReranker_Rerank_TopKAndThreshold(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Five input results; similarities will be replaced by rerank scores.
	results := []database.SearchResult{
		{Document: database.Document{SourcePath: "a.txt"}, Similarity: 0.1},
		{Document: database.Document{SourcePath: "b.txt"}, Similarity: 0.2},
		{Document: database.Document{SourcePath: "c.txt"}, Similarity: 0.3},
		{Document: database.Document{SourcePath: "d.txt"}, Similarity: 0.4},
		{Document: database.Document{SourcePath: "e.txt"}, Similarity: 0.5},
	}

	// Only the top 3 will be reranked; scores below Threshold will be filtered.
	// Scores correspond to indices 0,1,2 respectively.
	scores := []float64{0.6, 0.2, 0.9}

	model := &fakeRerankingProvider{
		Config: base.Config{},
		scores: scores,
	}

	r, err := NewLLMReranker(Config{
		Model:     model,
		TopK:      3,
		Threshold: 0.5,
	})
	require.NoError(t, err)

	got, err := r.Rerank(ctx, "query", results)
	require.NoError(t, err)

	// Expected behavior:
	// - Top 3 results get scores [0.6, 0.2, 0.9]; index 1 is filtered by Threshold.
	// - Tail results (indices 3 and 4) are appended unchanged.
	// - Final list is sorted by new Similarity descending.
	//
	// So we expect indices in order: 2 (0.9), 0 (0.6), 4 (0.5), 3 (0.4).
	require.Len(t, got, 4)

	paths := []string{
		got[0].Document.SourcePath,
		got[1].Document.SourcePath,
		got[2].Document.SourcePath,
		got[3].Document.SourcePath,
	}
	assert.Equal(t, []string{"c.txt", "a.txt", "e.txt", "d.txt"}, paths)

	sims := []float64{
		got[0].Similarity,
		got[1].Similarity,
		got[2].Similarity,
		got[3].Similarity,
	}
	assert.Equal(t, []float64{0.9, 0.6, 0.5, 0.4}, sims)
}

func TestDMRReranker_Rerank_EmptyResults(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	model := &fakeRerankingProvider{
		Config: base.Config{},
	}

	r, err := NewLLMReranker(Config{
		Model: model,
	})
	require.NoError(t, err)

	got, err := r.Rerank(ctx, "query", nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDMRReranker_Rerank_ModelWithoutRerankingSupport(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	model := &fakeProviderWithoutRerank{
		Config: base.Config{},
	}

	r, err := NewLLMReranker(Config{
		Model: model,
	})
	require.NoError(t, err)

	_, err = r.Rerank(ctx, "query", []database.SearchResult{
		{Document: database.Document{SourcePath: "a.txt"}, Similarity: 0.1},
	})
	require.Error(t, err)
}

func TestDMRReranker_Rerank_WithCriteria(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	results := []database.SearchResult{
		{Document: database.Document{SourcePath: "a.txt", Content: "content a"}, Similarity: 0.5},
		{Document: database.Document{SourcePath: "b.txt", Content: "content b"}, Similarity: 0.6},
	}

	scores := []float64{0.8, 0.3}

	model := &fakeRerankingProvider{
		Config: base.Config{},
		scores: scores,
	}

	// Create reranker with custom criteria
	criteria := "Prioritize recent information and practical examples"
	r, err := NewLLMReranker(Config{
		Model:    model,
		Criteria: criteria,
	})
	require.NoError(t, err)

	got, err := r.Rerank(ctx, "test query", results)
	require.NoError(t, err)

	// Verify scores were applied correctly (sorted by new scores)
	require.Len(t, got, 2)
	assert.Equal(t, "a.txt", got[0].Document.SourcePath) // 0.8 score
	assert.Equal(t, "b.txt", got[1].Document.SourcePath) // 0.3 score
	assert.InDelta(t, 0.8, got[0].Similarity, 0.0001)
	assert.InDelta(t, 0.3, got[1].Similarity, 0.0001)
}
