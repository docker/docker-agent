package fusion

import (
	"cmp"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// Fusion defines the interface for combining results from multiple retrieval strategies
type Fusion interface {
	// Fuse combines results from multiple strategies into a single ranked list
	// strategyResults maps strategy name to its results
	Fuse(strategyResults map[string][]database.SearchResult) ([]database.SearchResult, error)
}

// Config holds configuration for fusion strategies
type Config struct {
	Strategy string             // "rrf", "weighted", "max"
	K        int                // RRF parameter
	Weights  map[string]float64 // Strategy weights
}

// New creates a fusion strategy based on configuration
func New(config Config) (Fusion, error) {
	switch config.Strategy {
	case "rrf", "reciprocal_rank_fusion", "":
		return NewReciprocalRankFusion(cmp.Or(config.K, 60)), nil

	case "weighted":
		if len(config.Weights) == 0 {
			return nil, errors.New("weighted fusion requires strategy weights")
		}
		return NewWeightedFusion(config.Weights), nil

	case "max":
		return NewMaxScoreFusion(), nil

	default:
		return nil, fmt.Errorf("unknown fusion strategy: %s", config.Strategy)
	}
}
