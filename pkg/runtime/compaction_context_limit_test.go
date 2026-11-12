package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// providerOptsProvider is a minimal provider used to test that
// [providerContextLimit] reads the user-supplied context_size from
// the resolved [latest.ModelConfig.ProviderOpts] map.
type providerOptsProvider struct {
	id   string
	opts map[string]any
}

func (p *providerOptsProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *providerOptsProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &mockStream{}, nil
}

func (p *providerOptsProvider) BaseConfig() base.Config {
	cfg := base.Config{
		ModelConfig: latest.ModelConfig{ProviderOpts: p.opts},
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
			assert.Equal(t, tt.want, providerContextLimit(p))
		})
	}
}

// TestProviderContextLimit_NilProvider verifies the helper handles a
// nil provider safely (returns 0). Belt-and-braces for callers that
// can't statically prove non-nil.
func TestProviderContextLimit_NilProvider(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int64(0), providerContextLimit(nil))
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

// TestCompactionContextLimit_PrefersProviderOpts verifies that an explicit
// user-supplied provider_opts.context_size is the authoritative limit, even
// when the models.dev catalogue has its own entry. This is what the user is
// asking for — DMR allocates exactly context_size bytes for the inference
// context, and a user setting a smaller-than-catalogue value (cost / memory
// tuning) wants compaction to respect that.
func TestCompactionContextLimit_PrefersProviderOpts(t *testing.T) {
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
	assert.Equal(t, int64(8192), got,
		"explicit provider_opts.context_size must take precedence over the catalogue")
}

// TestCompactionContextLimit_FallsBackToCatalogue verifies that when the
// user has not supplied context_size, the runtime uses the models.dev
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

	primary := &providerOptsProvider{id: "openai/gpt-5", opts: map[string]any{"context_size": 200_000}}
	compaction := &providerOptsProvider{id: "openai/gpt-4o-mini", opts: map[string]any{"context_size": 16_000}}
	root := agent.New("root", "test",
		agent.WithModel(primary),
		agent.WithCompactionModel(compaction),
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
		primary := &providerOptsProvider{id: "openai/gpt-5", opts: map[string]any{"context_size": 200_000}}
		root := agent.New("root", "test", agent.WithModel(primary))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(200_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})

	t.Run("smaller compaction model caps the trigger", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", opts: map[string]any{"context_size": 200_000}}
		compaction := &providerOptsProvider{id: "openai/gpt-4o-mini", opts: map[string]any{"context_size": 16_000}}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compaction))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(16_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})

	t.Run("larger compaction model leaves the primary trigger", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", opts: map[string]any{"context_size": 200_000}}
		compaction := &providerOptsProvider{id: "openai/big", opts: map[string]any{"context_size": 1_000_000}}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compaction))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(200_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})

	t.Run("unresolvable primary uses the compaction model limit", func(t *testing.T) {
		t.Parallel()
		// A local/DMR primary not catalogued and without provider_opts.context_size
		// yields primaryLimit==0; a catalogued compaction model still lets
		// compaction run against the compaction model's window.
		primary := &providerOptsProvider{id: "dmr/local-gguf"} // unresolvable
		compaction := &providerOptsProvider{id: "openai/gpt-4o-mini", opts: map[string]any{"context_size": 16_000}}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compaction))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(errorModelStore{err: errors.New("x")}))
		require.NoError(t, err)

		assert.Equal(t, int64(16_000), rt.effectiveContextLimit(t.Context(), root, 0))
	})

	t.Run("unresolvable compaction limit falls back to primary", func(t *testing.T) {
		t.Parallel()
		primary := &providerOptsProvider{id: "openai/gpt-5", opts: map[string]any{"context_size": 200_000}}
		compaction := &providerOptsProvider{id: "unknown/model"} // no opts, no catalogue entry
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compaction))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		assert.Equal(t, int64(200_000), rt.effectiveContextLimit(t.Context(), root, 200_000))
	})
}
