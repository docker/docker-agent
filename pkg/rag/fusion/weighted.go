package fusion

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// WeightedFusion combines results using weighted sum of strategy scores
// Each strategy's score is multiplied by its weight
type WeightedFusion struct {
	weights map[string]float64
}

// NewWeightedFusion creates a new weighted fusion strategy
func NewWeightedFusion(weights map[string]float64) *WeightedFusion {
	return &WeightedFusion{weights: weights}
}

// Fuse combines results using weighted scores
func (wf *WeightedFusion) Fuse(strategyResults map[string][]database.SearchResult) ([]database.SearchResult, error) {
	slog.Debug("[Weighted Fusion] Starting fusion",
		"num_strategies", len(strategyResults),
		"weights", wf.weights)

	if len(strategyResults) == 0 {
		slog.Debug("[Weighted Fusion] No strategy results to fuse")
		return []database.SearchResult{}, nil
	}

	// Validate weights
	for strategyName := range strategyResults {
		if _, hasWeight := wf.weights[strategyName]; !hasWeight {
			slog.Error("[Weighted Fusion] Missing weight for strategy", "strategy", strategyName)
			return nil, fmt.Errorf("missing weight for strategy: %s", strategyName)
		}
	}

	// Log what each strategy contributed
	for strategyName, results := range strategyResults {
		weight := wf.weights[strategyName]
		slog.Debug("[Weighted Fusion] Strategy results",
			"strategy", strategyName,
			"num_results", len(results),
			"weight", weight)
	}

	// Calculate weighted scores for each unique document
	docScores := make(map[string]*fusedDocument)

	for strategyName, results := range strategyResults {
		weight := wf.weights[strategyName]

		for rank, result := range results {
			docID := result.Document.SourcePath + "_" + strconv.Itoa(result.Document.ChunkIndex)

			if _, exists := docScores[docID]; !exists {
				docScores[docID] = &fusedDocument{
					Document:       result.Document,
					StrategyScores: make(map[string]float64),
					StrategyRanks:  make(map[string]int),
					FusionScore:    0,
				}
			}

			// Weighted sum: score * weight
			docScores[docID].FusionScore += result.Similarity * weight
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

		// Log top results with detailed breakdown
		if i < 5 {
			slog.Debug("[Weighted Fusion] Final ranking",
				"rank", i+1,
				"source", doc.Document.SourcePath,
				"chunk", doc.Document.ChunkIndex,
				"weighted_score", doc.FusionScore,
				"original_scores", doc.StrategyScores,
				"weights", wf.weights)
		}
	}

	if len(results) > 0 {
		slog.Debug("[Weighted Fusion] Fusion complete",
			"total_unique_docs", len(results),
			"top_score", results[0].Similarity)
	} else {
		slog.Debug("[Weighted Fusion] Fusion complete with no results",
			"total_unique_docs", 0)
	}

	return results, nil
}
