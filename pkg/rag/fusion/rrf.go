package fusion

import (
	"cmp"
	"log/slog"
	"slices"
	"strconv"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// ReciprocalRankFusion implements the RRF (Reciprocal Rank Fusion) algorithm
// RRF is strategy-agnostic and works well for combining results from different retrieval methods
//
// Formula: score(d) = Σ(1 / (k + rank(d)))
// where k is typically 60, and rank starts at 1 for the top result
//
// Reference: "Reciprocal Rank Fusion outperforms Condorcet and individual Rank Learning Methods"
// by Cormack, Clarke, and Buettcher (SIGIR 2009)
type ReciprocalRankFusion struct {
	k int // Smoothing parameter (default: 60)
}

// NewReciprocalRankFusion creates a new RRF fusion strategy
func NewReciprocalRankFusion(k int) *ReciprocalRankFusion {
	return &ReciprocalRankFusion{k: cmp.Or(k, 60)}
}

// Fuse combines results from multiple strategies using RRF
func (rrf *ReciprocalRankFusion) Fuse(strategyResults map[string][]database.SearchResult) ([]database.SearchResult, error) {
	slog.Debug("[RRF Fusion] Starting fusion",
		"num_strategies", len(strategyResults),
		"rrf_k", rrf.k)

	if len(strategyResults) == 0 {
		slog.Debug("[RRF Fusion] No strategy results to fuse")
		return []database.SearchResult{}, nil
	}

	// If only one strategy, return its results as-is
	if len(strategyResults) == 1 {
		for strategyName, results := range strategyResults {
			slog.Debug("[RRF Fusion] Single strategy, returning results as-is",
				"strategy", strategyName,
				"num_results", len(results))
			return results, nil
		}
	}

	// Log what each strategy contributed
	for strategyName, results := range strategyResults {
		slog.Debug("[RRF Fusion] Strategy results",
			"strategy", strategyName,
			"num_results", len(results))

		for i, result := range results {
			if i < 3 { // Log first 3 for debugging
				slog.Debug("[RRF Fusion] Strategy result detail",
					"strategy", strategyName,
					"rank", i+1,
					"source", result.Document.SourcePath,
					"chunk", result.Document.ChunkIndex,
					"original_score", result.Similarity)
			}
		}
	}

	// Calculate RRF scores for each unique document
	docScores := make(map[string]*fusedDocument)

	for strategyName, results := range strategyResults {
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

			// RRF formula: 1 / (k + rank)
			// rank starts at 1 for the first result
			rrfScore := 1.0 / float64(rrf.k+rank+1)

			docScores[docID].FusionScore += rrfScore
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
			Similarity: doc.FusionScore, // Use RRF score as "similarity"
		}

		// Log top results
		if i < 5 {
			slog.Debug("[RRF Fusion] Final ranking",
				"rank", i+1,
				"source", doc.Document.SourcePath,
				"chunk", doc.Document.ChunkIndex,
				"rrf_score", doc.FusionScore,
				"strategy_ranks", doc.StrategyRanks,
				"original_scores", doc.StrategyScores)
		}
	}

	if len(results) > 0 {
		slog.Debug("[RRF Fusion] Fusion complete",
			"total_unique_docs", len(results),
			"top_score", results[0].Similarity)
	} else {
		slog.Debug("[RRF Fusion] Fusion complete with no results",
			"total_unique_docs", 0)
	}

	return results, nil
}

type fusedDocument struct {
	Document       database.Document
	StrategyScores map[string]float64 // Original scores from each strategy
	StrategyRanks  map[string]int     // Rank in each strategy's results
	FusionScore    float64            // Combined score
}
