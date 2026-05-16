package fusion

import (
	"cmp"
	"log/slog"
	"slices"
	"strconv"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// MaxScoreFusion combines results by taking the maximum score for each document
// Useful when strategies use the same scoring scale
type MaxScoreFusion struct{}

// NewMaxScoreFusion creates a new max score fusion strategy
func NewMaxScoreFusion() *MaxScoreFusion {
	return &MaxScoreFusion{}
}

// Fuse combines results using maximum score
func (mf *MaxScoreFusion) Fuse(strategyResults map[string][]database.SearchResult) ([]database.SearchResult, error) {
	slog.Debug("[Max Fusion] Starting fusion",
		"num_strategies", len(strategyResults))

	if len(strategyResults) == 0 {
		slog.Debug("[Max Fusion] No strategy results to fuse")
		return []database.SearchResult{}, nil
	}

	// Log what each strategy contributed
	for strategyName, results := range strategyResults {
		slog.Debug("[Max Fusion] Strategy results",
			"strategy", strategyName,
			"num_results", len(results))
	}

	// Calculate max score for each unique document
	docScores := make(map[string]*fusedDocument)

	for strategyName, results := range strategyResults {
		for rank, result := range results {
			docID := result.Document.SourcePath + "_" + strconv.Itoa(result.Document.ChunkIndex)

			if _, exists := docScores[docID]; !exists {
				docScores[docID] = &fusedDocument{
					Document:       result.Document,
					StrategyScores: make(map[string]float64),
					StrategyRanks:  make(map[string]int),
					FusionScore:    result.Similarity,
				}
			} else if result.Similarity > docScores[docID].FusionScore {
				// Take maximum score
				docScores[docID].FusionScore = result.Similarity
			}

			docScores[docID].StrategyScores[strategyName] = result.Similarity
			docScores[docID].StrategyRanks[strategyName] = rank + 1
		}
	}

	// Convert map to slice and sort by fusion score
	fusedDocs := make([]*fusedDocument, 0, len(docScores))
	for _, doc := range docScores {
		fusedDocs = append(fusedDocs, doc)
	}

	slices.SortFunc(fusedDocs, func(a, b *fusedDocument) int {
		return cmp.Compare(b.FusionScore, a.FusionScore) // Descending order
	})

	// Convert back to SearchResult format
	results := make([]database.SearchResult, len(fusedDocs))
	for i, doc := range fusedDocs {
		results[i] = database.SearchResult{
			Document:   doc.Document,
			Similarity: doc.FusionScore,
		}

		// Log top results
		if i < 5 {
			slog.Debug("[Max Fusion] Final ranking",
				"rank", i+1,
				"source", doc.Document.SourcePath,
				"chunk", doc.Document.ChunkIndex,
				"max_score", doc.FusionScore,
				"all_scores", doc.StrategyScores,
				"appeared_in_strategies", len(doc.StrategyScores))
		}
	}

	if len(results) > 0 {
		slog.Debug("[Max Fusion] Fusion complete",
			"total_unique_docs", len(results),
			"top_score", results[0].Similarity)
	} else {
		slog.Debug("[Max Fusion] Fusion complete with no results",
			"total_unique_docs", 0)
	}

	return results, nil
}
