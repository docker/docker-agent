package prefetch

import (
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/rag/database"
)

const (
	defaultMaxEntries = 32
)

// Config controls the exact-repeat RAG query cache. The zero value disables it.
type Config struct {
	Enabled    bool
	MaxEntries int
}

func (c Config) withDefaults() Config {
	if c.MaxEntries <= 0 {
		c.MaxEntries = defaultMaxEntries
	}
	return c
}

// Prefetcher owns a bounded result cache for one RAG manager.
type Prefetcher struct {
	cfg Config

	mu    sync.Mutex
	cache map[string][]database.SearchResult
	order []string
}

// New creates a prefetcher. It returns nil when disabled so callers can keep
// the hot path branch small.
func New(cfg Config) *Prefetcher {
	if !cfg.Enabled {
		return nil
	}
	cfg = cfg.withDefaults()
	return &Prefetcher{
		cfg:   cfg,
		cache: make(map[string][]database.SearchResult, cfg.MaxEntries),
	}
}

// Get returns cached final results for the exact normalized query.
func (p *Prefetcher) Get(query string) ([]database.SearchResult, bool) {
	if p == nil {
		return nil, false
	}
	key := normalize(query)
	if key == "" {
		return nil, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	results, ok := p.cache[key]
	if !ok {
		return nil, false
	}
	return cloneResults(results), true
}

// Store records final post-processed results for query.
func (p *Prefetcher) Store(query string, results []database.SearchResult) {
	if p == nil || len(results) == 0 {
		return
	}
	key := normalize(query)
	if key == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.storeLocked(key, results)
}

func (p *Prefetcher) storeLocked(key string, results []database.SearchResult) {
	if _, exists := p.cache[key]; !exists {
		p.order = append(p.order, key)
	}
	p.cache[key] = cloneResults(results)

	for len(p.order) > p.cfg.MaxEntries {
		oldest := p.order[0]
		p.order = p.order[1:]
		delete(p.cache, oldest)
	}
}

// Clear drops cached and in-flight query state after index changes.
func (p *Prefetcher) Clear() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.cache)
	p.order = nil
}

func normalize(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

func cloneResults(results []database.SearchResult) []database.SearchResult {
	return slices.Clone(results)
}
