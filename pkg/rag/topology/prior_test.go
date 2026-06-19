package topology

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

func TestPriorReranksCurrentResultsWithSmallTopologyBoost(t *testing.T) {
	prior := NewPrior(Config{Enabled: true, Weight: 0.05, MaxSourceHistory: 8})
	prior.Observe("how does rag manager query work", []database.SearchResult{
		result("pkg/rag/manager.go", 0.91),
	})
	results := []database.SearchResult{
		result("pkg/model/provider/client.go", 0.72),
		result("pkg/rag/manager.go", 0.70),
	}

	got := prior.Apply("rag manager cache behavior", results)

	require.Len(t, got, 2)
	assert.Equal(t, "pkg/rag/manager.go", got[0].Document.SourcePath)
	assert.Greater(t, got[0].Similarity, 0.70)
	assert.LessOrEqual(t, got[0].Similarity, 0.75)
	assert.Equal(t, "pkg/model/provider/client.go", got[1].Document.SourcePath)
}

func TestDisabledPriorReturnsResultsUnchanged(t *testing.T) {
	prior := NewPrior(Config{})
	results := []database.SearchResult{
		result("pkg/rag/manager.go", 0.70),
		result("pkg/model/provider/client.go", 0.72),
	}

	got := prior.Apply("rag manager cache behavior", results)

	assert.Equal(t, results, got)
}

func TestClearDropsSourceHistory(t *testing.T) {
	prior := NewPrior(Config{Enabled: true, Weight: 0.05})
	prior.Observe("how does rag manager query work", []database.SearchResult{
		result("pkg/rag/manager.go", 0.91),
	})

	prior.Clear()
	got := prior.Apply("rag manager cache behavior", []database.SearchResult{
		result("pkg/model/provider/client.go", 0.72),
		result("pkg/rag/manager.go", 0.70),
	})

	require.Len(t, got, 2)
	assert.Equal(t, "pkg/model/provider/client.go", got[0].Document.SourcePath)
}

func result(path string, similarity float64) database.SearchResult {
	return database.SearchResult{
		Document: database.Document{
			SourcePath: path,
			Content:    "content",
		},
		Similarity: similarity,
	}
}
