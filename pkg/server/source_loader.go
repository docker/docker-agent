package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config"
)

var sourceRetrySchedule = []time.Duration{2 * time.Second, 15 * time.Second, 70 * time.Second}

// sourceLoader must keep satisfying config.EncryptedConfigSource: it decorates
// every agent source in API-server mode, and teamloader discovers the agent
// config envelope through a type assertion, so dropping the method would
// silently stop forwarding it to the models gateway rather than fail to build.
var _ config.EncryptedConfigSource = (*sourceLoader)(nil)

type sourceLoader struct {
	inner           config.Source
	refreshInterval time.Duration

	mu   sync.RWMutex
	data []byte
	err  error
}

func newSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	sl := &sourceLoader{
		inner:           inner,
		refreshInterval: refreshInterval,
	}

	sl.load(ctx)

	if sl.hasError() {
		go sl.retryStartup(ctx)
	}

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

// EncryptedConfig forwards the inner source's captured agent config envelope so
// this caching decorator stays transparent to callers that type-assert for
// [config.EncryptedConfigSource] — chiefly teamloader, which adopts the value
// into RuntimeConfig.EncryptedConfig so it is forwarded to a trusted Docker
// models gateway on every model request.
//
// Without this passthrough the capability is silently lost in API-server mode
// (`docker agent serve api`, i.e. Docker Desktop): NewSessionManager wraps every
// source in a sourceLoader, so teamloader's assertion fails, no config envelope
// is forwarded, and the gateway's prompt verification degrades to a no-op — it
// fails open on a request that carries no config, so nothing surfaces as an
// error. The CLI path is unaffected because it hands the source over undecorated.
// See also the identical passthrough on the HCL decorator (pkg/config/hcl).
//
// It reads through to the inner source rather than caching the value: the config
// envelope is refreshed by the very same Read that refreshLoop performs, so a
// tag repushed under a new signature is picked up without a restart. Returns ""
// when the inner source does not support the capability.
func (sl *sourceLoader) EncryptedConfig() string {
	if ecs, ok := sl.inner.(config.EncryptedConfigSource); ok {
		return ecs.EncryptedConfig()
	}
	return ""
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

func (sl *sourceLoader) hasError() bool {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.err != nil
}

func (sl *sourceLoader) retryStartup(ctx context.Context) {
	for _, delay := range sourceRetrySchedule {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		sl.load(ctx)
		if !sl.hasError() {
			return
		}
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
