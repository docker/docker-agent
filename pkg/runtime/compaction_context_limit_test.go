package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func windowPtr(n int64) *int64 { return new(n) }

// providerOptsProvider exposes resolved model capacity for runtime tests.
type providerOptsProvider struct {
	id            string
	opts          map[string]any
	contextWindow *int64
}

func (p *providerOptsProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *providerOptsProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &mockStream{}, nil
}

func (p *providerOptsProvider) BaseConfig() base.Config {
	providerType, _, _ := strings.Cut(p.id, "/")
	cfg := base.Config{
		ModelConfig: latest.ModelConfig{Provider: providerType, ProviderOpts: p.opts, ContextWindow: p.contextWindow},
	}
	cfg.RebuildProvider = func(context.Context, *latest.ModelConfig, ...options.Opt) (provider.Provider, error) { return p, nil }
	return cfg
}

func (p *providerOptsProvider) MaxTokens() int { return 0 }

// TestProviderContextLimit covers the fallback that lets compaction
// trigger for local models that aren't catalogued in models.dev. The
// helper accepts the various scalar shapes that YAML/JSON decoders
// produce ("32768", 32768, 32768.0) and rejects junk.
func TestProviderContextLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want int64
	}{
		{name: "nil opts", opts: nil, want: 0},
		{name: "empty opts", opts: map[string]any{}, want: 0},
		{name: "missing key", opts: map[string]any{"other": 123}, want: 0},
		{name: "int", opts: map[string]any{"context_size": 32768}, want: 32768},
		{name: "int64", opts: map[string]any{"context_size": int64(65536)}, want: 65536},
		{name: "uint64 (yaml)", opts: map[string]any{"context_size": uint64(262144)}, want: 262144},
		{name: "float64 (json)", opts: map[string]any{"context_size": float64(8192)}, want: 8192},
		{name: "string decimal", opts: map[string]any{"context_size": "16384"}, want: 16384},
		{name: "string with whitespace", opts: map[string]any{"context_size": "  4096 "}, want: 4096},
		{name: "non-numeric string", opts: map[string]any{"context_size": "lots"}, want: 0},
		{name: "negative", opts: map[string]any{"context_size": -1}, want: 0},
		{name: "zero", opts: map[string]any{"context_size": 0}, want: 0},
		{name: "bool", opts: map[string]any{"context_size": true}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &providerOptsProvider{id: "dmr/test-model", opts: tt.opts}
			assert.Equal(t, tt.want, declaredContextLimit(p))
		})
	}
}

// TestDeclaredContextLimit_ModelLevelContextWindow verifies the model-level
// context_window is read as the declared window, and that it wins over an
// inconsistent provider_opts value for programmatic configs.
func TestDeclaredContextLimit_ModelLevelContextWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		id            string
		contextWindow *int64
		opts          map[string]any
		want          int64
	}{
		{name: "model-level only", id: "custom/model", contextWindow: windowPtr(200_000), want: 200_000},
		{name: "model-level beats DMR provider_opts", id: "dmr/ai/qwen3", contextWindow: windowPtr(200_000), opts: map[string]any{"context_size": 8192}, want: 200_000},
		{name: "DMR provider_opts only", id: "dmr/ai/qwen3", opts: map[string]any{"context_size": 8192}, want: 8192},
		{name: "non-positive model-level falls back to DMR provider_opts", id: "dmr/ai/qwen3", contextWindow: windowPtr(0), opts: map[string]any{"context_size": 8192}, want: 8192},
		{name: "provider_opts ignored outside DMR", id: "custom/model", opts: map[string]any{"context_size": 8192}, want: 0},
		{name: "neither", id: "custom/model", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &providerOptsProvider{id: tt.id, opts: tt.opts, contextWindow: tt.contextWindow}
			assert.Equal(t, tt.want, declaredContextLimit(p))
		})
	}
}

// TestCompactionContextLimit_ModelLevelContextWindow verifies that a custom
// endpoint with no catalogue entry (e.g. openai_responses) gets its compaction
// window from models.<name>.context_window, with no provider_opts involved.
func TestCompactionContextLimit_ModelLevelContextWindow(t *testing.T) {
	t.Parallel()

	size := int64(128_000)
	prov := &providerOptsProvider{id: "gateway/my-hosted-model", contextWindow: &size}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("not in catalogue")}))
	require.NoError(t, err)

	assert.Equal(t, int64(128_000), rt.compactionContextLimit(t.Context(), root))
	assert.Equal(t, int64(128_000), rt.resolveContextLimit(t.Context(), prov, prov.ID()))
}

// TestProviderContextLimit_NilProvider verifies the helper handles a
// nil provider safely (returns 0). Belt-and-braces for callers that
// can't statically prove non-nil.
func TestProviderContextLimit_NilProvider(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int64(0), declaredContextLimit(nil))
}

// errorModelStore returns a "not found" error from GetModel, simulating
// a models.dev catalogue that doesn't have an entry for the configured
// model (the exact case reported for DMR + HuggingFace GGUF models).
type errorModelStore struct {
	ModelStore

	err error
}

func (s errorModelStore) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return nil, s.err
}

// TestCompactionContextLimit_FallsBackToProviderOpts verifies that the
// runtime resolves a usable context limit from provider_opts.context_size
// when the models.dev catalogue lookup fails.
//
// This is the core of the fix for the reported bug: DMR users with a
// model not catalogued in models.dev (e.g. a HuggingFace GGUF) could
// supply context_size via provider_opts but compaction silently became
// a no-op, eventually surfacing as "Failed to get model definition"
// when overflow recovery was attempted.
func TestCompactionContextLimit_FallsBackToProviderOpts(t *testing.T) {
	t.Parallel()

	prov := &providerOptsProvider{
		id:   "dmr/hf.co/unsloth/qwen3-4b-gguf:Q4_K_M",
		opts: map[string]any{"context_size": 32768},
	}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("not in catalogue")}))
	require.NoError(t, err)

	got := rt.compactionContextLimit(t.Context(), root)
	assert.Equal(t, int64(32768), got,
		"context limit must fall back to provider_opts.context_size when models.dev has no entry")
}

// TestCompactionContextLimit_IgnoresProviderOptsOutsideDMR verifies that
// provider_opts.context_size is DMR-only: on any other provider it does not
// override the catalogue, which is what a stale or misplaced value would
// otherwise silently do. Validation rejects it at load time; this covers the
// runtime if such a config is built programmatically.
func TestCompactionContextLimit_IgnoresProviderOptsOutsideDMR(t *testing.T) {
	t.Parallel()

	prov := &providerOptsProvider{
		id:   "openai/gpt-5",
		opts: map[string]any{"context_size": 8192},
	}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	got := rt.compactionContextLimit(t.Context(), root)
	assert.Equal(t, int64(200_000), got,
		"provider_opts.context_size must not set the window for a non-DMR provider")
}

// TestCompactionContextLimit_FallsBackToCatalogue verifies that when the
// user has not supplied context_window, the runtime uses the models.dev
// catalogue limit. This is the path most hosted-model users hit.
func TestCompactionContextLimit_FallsBackToCatalogue(t *testing.T) {
	t.Parallel()

	prov := &providerOptsProvider{id: "openai/gpt-5"} // no opts
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStoreWithLimit{limit: 200_000}))
	require.NoError(t, err)

	got := rt.compactionContextLimit(t.Context(), root)
	assert.Equal(t, int64(200_000), got)
}

// TestCompactionContextLimit_NoSourcesYieldsZero verifies the legacy
// behaviour: when neither models.dev nor provider_opts provides a
// limit, the function returns 0 (callers treat this as "can't
// compact"; the LLM strategy enforces ContextLimit > 0).
func TestCompactionContextLimit_NoSourcesYieldsZero(t *testing.T) {
	t.Parallel()

	prov := &providerOptsProvider{id: "unknown/model"} // no opts
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	got := rt.compactionContextLimit(t.Context(), root)
	assert.Equal(t, int64(0), got)
}

// TestCompactionContextLimit_UsesCompactionModel verifies that when a
// dedicated compaction model is configured, the resolved context limit comes
// from that model and not from the agent's (larger) primary model. This is the
// budget the summary call is scaled against (issue #3241).
func TestCompactionContextLimit_UsesCompactionModel(t *testing.T) {
	t.Parallel()

	primary := &providerOptsProvider{id: "openai/gpt-5", contextWindow: windowPtr(200_000)}
	summarizer := &providerOptsProvider{id: "openai/gpt-4o-mini", contextWindow: windowPtr(16_000)}
	root := agent.New("root", "test",
		agent.WithModel(primary),
		agent.WithCompactionModel(summarizer),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("not in catalogue")}))
	require.NoError(t, err)

	got := rt.compactionContextLimit(t.Context(), root)
	assert.Equal(t, int64(16_000), got,
		"context limit must resolve from the dedicated compaction model, not the primary")
}

// TestEffectiveContextLimit covers the budget that drives both the proactive
// compaction trigger and the UI context gauge. With no dedicated compaction
// model it equals the primary limit; with one it is min(primary, compaction)
// so compaction fires while the conversation still fits the compaction model's
// window. An unresolvable compaction-model limit falls back to the primary so
// a misconfigured compaction model never suppresses compaction.
func TestEffectiveContextLimit(t *testing.T) {
	t.Parallel()

	t.Run("no compaction model uses primary", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", contextWindow: windowPtr(200_000)}
		root := agent.New("root", "test", agent.WithModel(primary))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(200_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})

	t.Run("smaller compaction model caps the trigger", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", contextWindow: windowPtr(200_000)}
		summarizer := &providerOptsProvider{id: "openai/gpt-4o-mini", contextWindow: windowPtr(16_000)}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(16_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})

	t.Run("larger compaction model leaves the primary trigger", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", contextWindow: windowPtr(200_000)}
		summarizer := &providerOptsProvider{id: "openai/big", contextWindow: windowPtr(1_000_000)}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(200_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
		assert.False(t, compactionCaps(200_000, rt.compactionContextLimit(t.Context(), root)))
	})

	t.Run("unresolvable primary uses the compaction model limit", func(t *testing.T) {
		t.Parallel()
		// A local/DMR primary not catalogued and without provider_opts.context_size
		// yields primaryLimit==0; a catalogued compaction model still lets
		// compaction run against the compaction model's window.
		primary := &providerOptsProvider{id: "dmr/local-gguf"} // unresolvable
		summarizer := &providerOptsProvider{id: "openai/gpt-4o-mini", contextWindow: windowPtr(16_000)}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(16_000), rt.effectiveContextLimit(t.Context(), root, 0))
	})

	t.Run("unresolvable compaction limit falls back to primary", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", contextWindow: windowPtr(200_000)}
		summarizer := &providerOptsProvider{id: "unknown/model"} // no opts, no catalogue entry
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		assert.Equal(t, int64(200_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})
}

func TestContextWindowCapacityDrivesCompactionThresholds(t *testing.T) {
	t.Parallel()

	const window = int64(100_000)
	prov := &providerOptsProvider{id: "gateway/hosted", contextWindow: windowPtr(window)}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(errorModelStore{}))
	require.NoError(t, err)

	limit := rt.effectiveContextLimit(t.Context(), root, rt.resolveContextLimit(t.Context(), prov, prov.ID()))
	require.Equal(t, window, limit, "context_window is the capacity the thresholds apply to")

	assert.False(t, compaction.ShouldCompact(89_999, 0, 0, limit, compaction.DefaultThreshold))
	assert.True(t, compaction.ShouldCompact(90_001, 0, 0, limit, compaction.DefaultThreshold))
}

// A smaller dedicated compaction window caps the effective limit, so the
// proactive trigger fires against it. Usage that is far below the primary
// window's threshold but above the compaction window's threshold must compact.
func TestContextWindowSmallerCompactionModelCapsTrigger(t *testing.T) {
	t.Parallel()

	const (
		primaryWindow    = int64(200_000)
		compactionWindow = int64(32_000)
	)
	primary := &providerOptsProvider{id: "gateway/big", contextWindow: windowPtr(primaryWindow)}
	summarizer := &providerOptsProvider{id: "dmr/ai/qwen3", contextWindow: windowPtr(compactionWindow)}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(errorModelStore{}))
	require.NoError(t, err)

	primaryLimit := rt.resolveContextLimit(t.Context(), primary, primary.ID())
	require.Equal(t, primaryWindow, primaryLimit)

	effective := rt.effectiveContextLimit(t.Context(), root, primaryLimit)
	assert.Equal(t, compactionWindow, rt.compactionContextLimit(t.Context(), root), "summary model window")
	assert.Equal(t, compactionWindow, effective, "the smaller compaction window caps the limit")

	const usage = int64(30_000) // 0.9 of 200k is 180k; 0.9 of 32k is 28.8k
	assert.False(t, compaction.ShouldCompact(usage, 0, 0, primaryLimit, compaction.DefaultThreshold),
		"against the primary window alone this usage would not compact")
	assert.True(t, compaction.ShouldCompact(usage, 0, 0, effective, compaction.DefaultThreshold),
		"the capped limit must drive the trigger")

	model, primaryCap, compactionCap := rt.compactionCapAttribution(t.Context(), root, primary.ID())
	assert.Equal(t, "dmr/ai/qwen3", model)
	assert.Equal(t, primaryWindow, primaryCap)
	assert.Equal(t, compactionWindow, compactionCap)
	assert.True(t, compactionCaps(primaryCap, compactionCap), "the cap is attributed to the compaction model")
}
