package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config"
)

type sourceLoader struct {
	inner           config.Source
	refreshInterval time.Duration

	mu   sync.RWMutex
	data []byte
	err  error
}

// NewSourceLoader creates a new source loader that caches and periodically refreshes a config source.
func NewSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	return newSourceLoader(ctx, inner, refreshInterval)
}

func newSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	sl := &sourceLoader{
		inner:           inner,
		refreshInterval: refreshInterval,
	}

	sl.load(ctx)

	if refreshInterval > 0 {
		go sl.refreshLoop(ctx)
	}

	return sl
}

func (sl *sourceLoader) Name() string {
	return sl.inner.Name()
}

func (sl *sourceLoader) ParentDir() string {
	return sl.inner.ParentDir()
}

func (sl *sourceLoader) Read(_ context.Context) ([]byte, error) {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.data, sl.err
}

func (sl *sourceLoader) load(ctx context.Context) {
	data, err := sl.inner.Read(ctx)

	sl.mu.Lock()
	defer sl.mu.Unlock()

	if err != nil {
		// Only log errors, keep previous data if available
		slog.WarnContext(ctx, "Failed to refresh source",
			"source", sl.inner.Name(),
			"error", err)
		// Only update error if we don't have data yet
		if len(sl.data) == 0 {
			sl.err = err
		}
	} else {
		sl.data = data
		sl.err = nil
	}
}

func (sl *sourceLoader) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(sl.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sl.load(ctx)
		}
	}
}
