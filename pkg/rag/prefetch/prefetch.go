package prefetch

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/rag/database"
)

const (
	defaultMaxEntries     = 32
	defaultMaxCandidates  = 2
	defaultMinSimilarity  = 0.5
	defaultDriftThreshold = 0.8
	defaultTimeout        = 10 * time.Second
)

// Config controls adaptive RAG prefetching. The zero value disables it.
type Config struct {
	Enabled        bool
	MaxEntries     int
	MaxCandidates  int
	MinSimilarity  float64
	DriftThreshold float64
	Timeout        time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxEntries <= 0 {
		c.MaxEntries = defaultMaxEntries
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = defaultMaxCandidates
	}
	if c.MinSimilarity <= 0 {
		c.MinSimilarity = defaultMinSimilarity
	}
	if c.DriftThreshold <= 0 {
		c.DriftThreshold = defaultDriftThreshold
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	return c
}

// FetchFunc runs an uncached query for a prefetch candidate.
type FetchFunc func(context.Context, string) ([]database.SearchResult, error)

// Prefetcher owns bounded result cache, lightweight topology state, and
// background prefetch scheduling for one RAG manager.
type Prefetcher struct {
	cfg Config

	mu       sync.Mutex
	cache    map[string]cacheEntry
	order    []string
	inflight map[string]struct{}
	tracker  tracker
}

type cacheEntry struct {
	results []database.SearchResult
	anchors []string
}

// New creates a prefetcher. It returns nil when disabled so callers can keep
// the hot path branch small.
func New(cfg Config) *Prefetcher {
	if !cfg.Enabled {
		return nil
	}
	cfg = cfg.withDefaults()
	return &Prefetcher{
		cfg:      cfg,
		cache:    make(map[string]cacheEntry, cfg.MaxEntries),
		inflight: make(map[string]struct{}),
	}
}

// Get returns cached final results for query.
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
		return p.getTopologyLocked(key)
	}
	return cloneResults(results.results), true
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
	p.cache[key] = cacheEntry{
		results: cloneResults(results),
		anchors: anchorsFor(results),
	}

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
	clear(p.inflight)
	p.order = nil
}

func (p *Prefetcher) getTopologyLocked(key string) ([]database.SearchResult, bool) {
	if !p.tracker.stable(p.cfg.DriftThreshold) {
		return nil, false
	}
	queryTokens := tokenSet(key)
	for _, entryKey := range slices.Backward(p.order) {
		entry := p.cache[entryKey]
		if !topologyRelated(entryKey, queryTokens, entry.anchors) {
			continue
		}
		return cloneResults(entry.results), true
	}
	return nil, false
}

// Observe updates the topology tracker with query/result metadata.
func (p *Prefetcher) Observe(query string, results []database.SearchResult) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tracker.observe(query, results)
}

// Candidates returns deterministic follow-up queries worth warming.
func (p *Prefetcher) Candidates(query string, results []database.SearchResult) []string {
	if p == nil || len(results) == 0 {
		return nil
	}

	p.mu.Lock()
	stable := p.tracker.stable(p.cfg.DriftThreshold)
	p.mu.Unlock()
	if !stable {
		return nil
	}

	base := normalize(query)
	baseTokens := tokenSet(base)
	seen := map[string]struct{}{base: {}}
	candidates := make([]string, 0, p.cfg.MaxCandidates)

	for _, result := range results {
		if len(candidates) >= p.cfg.MaxCandidates {
			break
		}
		if result.Similarity < p.cfg.MinSimilarity {
			continue
		}
		suffix := sourceSuffix(result.Document.SourcePath, baseTokens)
		if suffix == "" {
			continue
		}
		candidate := strings.TrimSpace(base + " " + suffix)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}

	return candidates
}

// Prefetch schedules one bounded background fetch for query.
func (p *Prefetcher) Prefetch(ctx context.Context, query string, fetch FetchFunc) {
	if p == nil || fetch == nil {
		return
	}
	key := normalize(query)
	if key == "" {
		return
	}

	p.mu.Lock()
	if _, ok := p.cache[key]; ok {
		p.mu.Unlock()
		return
	}
	if _, ok := p.inflight[key]; ok {
		p.mu.Unlock()
		return
	}
	p.inflight[key] = struct{}{}
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inflight, key)
			p.mu.Unlock()
		}()

		prefetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.Timeout)
		defer cancel()

		results, err := fetch(prefetchCtx, key)
		if err != nil || len(results) == 0 {
			return
		}

		p.mu.Lock()
		p.storeLocked(key, results)
		p.mu.Unlock()
	}()
}

func normalize(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

func sourceToken(path string) string {
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

func sourceSuffix(path string, seen map[string]struct{}) string {
	parts := strings.FieldsFunc(sourceToken(path), isTokenSeparator)
	novel := make([]string, 0, len(parts))
	for _, part := range parts {
		part = normalize(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		novel = append(novel, part)
	}
	return strings.Join(novel, " ")
}

func anchorsFor(results []database.SearchResult) []string {
	seen := map[string]struct{}{}
	anchors := make([]string, 0, len(results))
	for _, result := range results {
		for _, part := range strings.FieldsFunc(sourceToken(result.Document.SourcePath), isTokenSeparator) {
			part = normalize(part)
			if len(part) < 3 {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			anchors = append(anchors, part)
		}
	}
	return anchors
}

func isTokenSeparator(r rune) bool {
	return r == '_' || r == '-' || r == '.'
}

func topologyRelated(entryKey string, queryTokens map[string]struct{}, anchors []string) bool {
	hasAnchor := false
	for _, anchor := range anchors {
		if _, ok := queryTokens[anchor]; ok {
			hasAnchor = true
			break
		}
	}
	if !hasAnchor {
		return false
	}
	return jaccard(tokenSet(entryKey), queryTokens) >= 0.25
}

func tokenSet(query string) map[string]struct{} {
	tokens := map[string]struct{}{}
	for token := range strings.FieldsSeq(query) {
		tokens[token] = struct{}{}
	}
	return tokens
}

func jaccard(a, b map[string]struct{}) float64 {
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

func cloneResults(results []database.SearchResult) []database.SearchResult {
	return slices.Clone(results)
}

type tracker struct {
	seen     int
	centroid [4]float64
	drift    float64
}

func (t *tracker) observe(query string, results []database.SearchResult) {
	point := pointFor(query, results)
	t.seen++
	if t.seen == 1 {
		t.centroid = point
		t.drift = 0
		return
	}

	var dist float64
	for i := range point {
		diff := point[i] - t.centroid[i]
		dist += diff * diff
	}
	t.drift = dist

	for i := range t.centroid {
		t.centroid[i] += (point[i] - t.centroid[i]) * 0.35
	}
}

func (t *tracker) stable(threshold float64) bool {
	if t.seen < 2 {
		return true
	}
	return t.drift <= threshold
}

func pointFor(query string, results []database.SearchResult) [4]float64 {
	var avgSimilarity float64
	for _, result := range results {
		avgSimilarity += result.Similarity
	}
	if len(results) > 0 {
		avgSimilarity /= float64(len(results))
	}
	return [4]float64{
		float64(len(query)) / 256,
		float64(len(strings.Fields(query))) / 32,
		float64(len(results)) / 32,
		avgSimilarity,
	}
}
