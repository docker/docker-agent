package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

type contextProvider struct {
	configProvider

	lookup func(context.Context) (int64, error)
}

func (p *contextProvider) ContextWindow(ctx context.Context) (int64, error) { return p.lookup(ctx) }

func newContextProvider(endpoint string, lookup func(context.Context) (int64, error)) *contextProvider {
	return &contextProvider{
		configProvider: configProvider{Config: base.Config{BaseURL: endpoint, ModelConfig: latest.ModelConfig{Provider: "dmr", Model: "ai/qwen3"}}},
		lookup:         lookup,
	}
}

func TestResolveContextLimitDiscovery(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		explicit   int
		discovered int64
		err        error
		want       int64
		calls      int
	}{
		{name: "explicit wins", explicit: 4096, discovered: 8192, want: 4096},
		{name: "discovery before catalog", discovered: 8192, want: 8192, calls: 1},
		{name: "missing metadata falls back", want: 32768, calls: 1},
		{name: "failed discovery falls back", err: errors.New("unreachable"), want: 32768, calls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			p := newContextProvider("http://local/engines/v1", func(context.Context) (int64, error) { calls++; return tt.discovered, tt.err })
			if tt.explicit > 0 {
				p.ModelConfig.ProviderOpts = map[string]any{"context_size": tt.explicit}
			}
			r := &LocalRuntime{modelsStore: stubModelStore{models: map[string]*modelsdev.Model{"dmr/ai/qwen3": {Limit: modelsdev.Limit{Context: 32768}}}}}
			for range 3 {
				assert.Equal(t, tt.want, r.resolveContextLimit(t.Context(), p, p.ID()))
			}
			assert.Equal(t, tt.calls, calls)
			if tt.explicit == 0 {
				assert.Nil(t, p.ModelConfig.ProviderOpts, "discovery must not rewrite configuration")
			}
		})
	}
}

func TestDiscoveredContextCacheScopeAndExpiry(t *testing.T) {
	t.Parallel()
	calls := 0
	lookup := func(context.Context) (int64, error) { calls++; return 8192, nil }
	now := time.Now()
	r := &LocalRuntime{now: func() time.Time { return now }}
	p := newContextProvider("http://one/engines/v1", lookup)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), p))
	// A rebuilt provider at the same endpoint reuses the result.
	clone := newContextProvider("http://one/engines/v1", lookup)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), clone))
	assert.Equal(t, 1, calls)
	other := newContextProvider("http://two/engines/v1", lookup)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), other))
	assert.Equal(t, 2, calls)
	clone.ModelConfig.ProviderOpts = map[string]any{"runtime_flags": []string{"-c", "4096"}}
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), clone))
	assert.Equal(t, 3, calls, "different runtime options must not reuse an allocation")
	now = now.Add(time.Minute)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 4, calls)
}

func TestDiscoveredContextDoesNotCacheCancellation(t *testing.T) {
	t.Parallel()
	calls := 0
	p := newContextProvider("http://local", func(ctx context.Context) (int64, error) {
		calls++
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 4096, nil
	})
	r := &LocalRuntime{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Zero(t, r.discoveredContextLimit(ctx, p))
	assert.Equal(t, int64(4096), r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 2, calls)
}

func TestDiscoveredContextConcurrent(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	p := newContextProvider("http://local", func(context.Context) (int64, error) { calls.Add(1); return 4096, nil })
	r := &LocalRuntime{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { assert.Equal(t, int64(4096), r.discoveredContextLimit(t.Context(), p)) })
	}
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load())
}

func TestCompactionUsesDiscoveredWindow(t *testing.T) {
	t.Parallel()
	p := newContextProvider("http://local", func(context.Context) (int64, error) { return 8192, nil })
	a := agent.New("root", "test", agent.WithModel(p))
	r := &LocalRuntime{modelsStore: stubModelStore{}}
	require.Equal(t, int64(8192), r.compactionContextLimit(t.Context(), a))
}
