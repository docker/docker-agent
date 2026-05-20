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

	// ready is closed when the first load attempt has completed (whether it
	// produced data, an error, or was cancelled). Read blocks on this so
	// callers never observe the pre-load (nil, nil) state, preserving the
	// contract of the previous synchronous-constructor implementation.
	ready chan struct{}

	mu   sync.RWMutex
	data []byte
	err  error
}

// NewSourceLoader creates a new source loader that caches and periodically refreshes a config source.
// The initial load runs in a goroutine; callers of Read block until the first load completes.
func NewSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	return newSourceLoader(ctx, inner, refreshInterval)
}

func newSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	sl := &sourceLoader{
		inner:           inner,
		refreshInterval: refreshInterval,
		ready:           make(chan struct{}),
	}

	// Run the initial load asynchronously so the constructor returns
	// immediately. This lets the HTTP server start serving non-source
	// endpoints (e.g. /api/ping, /health) while a potentially slow OCI
	// pull or HTTPS GET is still in flight. Endpoints that need the
	// source content (getAgents, createSession, ...) block in Read
	// until this load finishes.
	go func() {
		defer close(sl.ready)
		sl.load(ctx)
	}()

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

// Read returns the latest cached data, blocking until the first load attempt
// has completed or ctx is cancelled. Once the first load has finished, Read
// returns the cached data/err snapshot without further blocking; the
// background refreshLoop is responsible for keeping the snapshot fresh.
func (sl *sourceLoader) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-sl.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
