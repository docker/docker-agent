package prefetch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

func TestDisabledPrefetcher(t *testing.T) {
	assert.Nil(t, New(Config{}))
}

func TestStoreGetAndEvictOldest(t *testing.T) {
	p := New(Config{Enabled: true, MaxEntries: 2})

	p.Store("Alpha Query", result("a.go", 0.9))
	p.Store("Beta Query", result("b.go", 0.9))
	p.Store("Gamma Query", result("c.go", 0.9))

	_, ok := p.Get("alpha query")
	assert.False(t, ok)
	_, ok = p.Get("beta query")
	assert.True(t, ok)
	got, ok := p.Get("GAMMA   query")
	require.True(t, ok)
	assert.Equal(t, "c.go", got[0].Document.SourcePath)
}

func TestGetOnlyMatchesExactNormalizedQuery(t *testing.T) {
	p := New(Config{Enabled: true})
	p.Store("how does rag manager query work", []database.SearchResult{
		result("pkg/rag/manager.go", 0.92)[0],
		result("pkg/rag/prefetch/prefetch.go", 0.76)[0],
	})

	_, ok := p.Get("rag manager cache behavior")

	assert.False(t, ok)
}

func TestStoreAndGetCloneResults(t *testing.T) {
	p := New(Config{Enabled: true})
	results := result("docs/rag.md", 0.9)

	p.Store("RAG cache", results)
	results[0].Document.Content = "mutated after store"

	got, ok := p.Get("rag cache")
	require.True(t, ok)
	got[0].Document.Content = "mutated after get"

	again, ok := p.Get("rag cache")
	require.True(t, ok)
	assert.Equal(t, "content", again[0].Document.Content)
}

func TestClearDropsCachedResults(t *testing.T) {
	p := New(Config{Enabled: true})
	p.Store("RAG cache", result("docs/rag.md", 0.9))

	p.Clear()

	_, ok := p.Get("RAG cache")
	assert.False(t, ok)
}

func result(path string, similarity float64) []database.SearchResult {
	return []database.SearchResult{{
		Document: database.Document{
			SourcePath: path,
			Content:    "content",
		},
		Similarity: similarity,
	}}
}
