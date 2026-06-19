package topology

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/rag/database"
)

const (
	defaultWeight           = 0.05
	defaultMaxSourceHistory = 32
	maxWeight               = 0.2
)

// Config controls the topology prior. The zero value disables it.
type Config struct {
	Enabled          bool
	Weight           float64
	MaxSourceHistory int
}

// Prior applies a small source-topology score to already-retrieved results.
type Prior struct {
	cfg Config

	mu      sync.Mutex
	sources []sourcePoint
}

type sourcePoint struct {
	path   string
	tokens map[string]struct{}
}

// NewPrior creates a disabled-by-default topology prior.
func NewPrior(cfg Config) *Prior {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Weight <= 0 {
		cfg.Weight = defaultWeight
	}
	cfg.Weight = math.Min(cfg.Weight, maxWeight)
	if cfg.MaxSourceHistory <= 0 {
		cfg.MaxSourceHistory = defaultMaxSourceHistory
	}
	return &Prior{cfg: cfg}
}

// Apply blends a capped topology score into the current query's retrieved results.
func (p *Prior) Apply(query string, results []database.SearchResult) []database.SearchResult {
	if p == nil || len(results) == 0 {
		return results
	}

	p.mu.Lock()
	history := slices.Clone(p.sources)
	p.mu.Unlock()

	queryTokens := tokenSet(query)
	scored := slices.Clone(results)
	for i := range scored {
		sourceTokens := sourceTokenSet(scored[i].Document.SourcePath)
		score := 0.7*jaccard(queryTokens, sourceTokens) + 0.3*historyScore(sourceTokens, history)
		scored[i].Similarity += p.cfg.Weight * score
	}
	slices.SortStableFunc(scored, func(a, b database.SearchResult) int {
		return cmp.Compare(b.Similarity, a.Similarity)
	})
	return scored
}

// Observe records source topology from completed foreground retrievals.
func (p *Prior) Observe(_ string, results []database.SearchResult) {
	if p == nil || len(results) == 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, result := range results {
		path := result.Document.SourcePath
		if path == "" || containsSource(p.sources, path) {
			continue
		}
		p.sources = append(p.sources, sourcePoint{
			path:   path,
			tokens: sourceTokenSet(path),
		})
	}
	for len(p.sources) > p.cfg.MaxSourceHistory {
		p.sources = p.sources[1:]
	}
}

// Clear drops topology history after index changes.
func (p *Prior) Clear() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources = nil
}

func containsSource(sources []sourcePoint, path string) bool {
	for _, source := range sources {
		if source.path == path {
			return true
		}
	}
	return false
}

func historyScore(tokens map[string]struct{}, history []sourcePoint) float64 {
	var best float64
	for _, source := range history {
		best = math.Max(best, jaccard(tokens, source.tokens))
	}
	return best
}

func tokenSet(text string) map[string]struct{} {
	tokens := map[string]struct{}{}
	for _, token := range strings.FieldsFunc(strings.ToLower(text), isTokenSeparator) {
		if len(token) < 2 {
			continue
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

func sourceTokenSet(path string) map[string]struct{} {
	return tokenSet(path)
}

func isTokenSeparator(r rune) bool {
	return r == '/' || r == '\\' || r == '.' || r == '-' || r == '_' || r == ' '
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var intersection int
	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
