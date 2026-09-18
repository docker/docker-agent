package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/docker/docker-agent/pkg/model/provider"
)

type modelContextKey struct {
	baseURL, provider, model, options string
}
type modelContextEntry struct {
	limit     int64
	fetchedAt time.Time
}

// Cache by endpoint and model so title/compaction clones share the lookup.
type modelContextCache struct {
	mu      sync.Mutex
	sf      singleflight.Group
	entries map[modelContextKey]modelContextEntry
}

func (r *LocalRuntime) discoveredContextLimit(ctx context.Context, p provider.Provider) int64 {
	if p == nil {
		return 0
	}
	resolve := provider.ContextWindowResolver(p)
	if resolve == nil {
		return 0
	}
	cfg := p.BaseConfig()
	opts, err := json.Marshal(cfg.ModelConfig.ProviderOpts)
	if err != nil {
		return 0
	}
	key := modelContextKey{cfg.BaseURL, cfg.ModelConfig.Provider, cfg.ModelConfig.Model, string(opts)}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	cache := &r.modelContexts
	read := func() (int64, bool) {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		entry, ok := cache.entries[key]
		return entry.limit, ok && now().Sub(entry.fetchedAt) < time.Minute
	}
	if limit, ok := read(); ok {
		return limit
	}
	value, _, _ := cache.sf.Do(key.baseURL+"\x00"+key.provider+"\x00"+key.model+"\x00"+key.options, func() (any, error) {
		if limit, ok := read(); ok {
			return limit, nil
		}
		limit, err := resolve(ctx)
		if ctx.Err() != nil {
			return int64(0), nil
		}
		if err != nil {
			slog.DebugContext(ctx, "Model context discovery failed", "model", p.ID(), "error", err)
			limit = 0
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		// Bound memory when sessions cycle through many custom model references.
		if cache.entries == nil || len(cache.entries) >= 64 {
			cache.entries = make(map[modelContextKey]modelContextEntry)
		}
		cache.entries[key] = modelContextEntry{limit: limit, fetchedAt: now()}
		return limit, nil
	})
	return value.(int64)
}
