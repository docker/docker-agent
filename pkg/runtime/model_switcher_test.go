package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockCatalogStore implements ModelStore for testing
type mockCatalogStore struct {
	ModelStore

	db *modelsdev.Database
}

func (m *mockCatalogStore) GetDatabase(_ context.Context) (*modelsdev.Database, error) {
	return m.db, nil
}

// configProvider is a provider.Provider whose BaseConfig is fully controlled
// by the test, used to drive thinking-level cycling without a real client.
type configProvider struct {
	base.Config
}

func (configProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("not implemented")
}

func newConfigProvider(cfg latest.ModelConfig) *configProvider {
	return &configProvider{Config: base.Config{ModelConfig: cfg}}
}

func testProviderRegistry() *provider.Registry {
	factory := func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		return newConfigProvider(*cfg), nil
	}
	return provider.NewRegistry(map[string]provider.Factory{
		"openai":                 factory,
		"openai_chatcompletions": factory,
		"openai_responses":       factory,
		"anthropic":              factory,
		"google":                 factory,
		"amazon-bedrock":         factory,
	})
}

func TestCurrentThinkingLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		budget *latest.ThinkingBudget
		want   effort.Level
	}{
		{"nil budget", nil, effort.None},
		{"disabled none", &latest.ThinkingBudget{Effort: "none"}, effort.None},
		{"disabled zero tokens", &latest.ThinkingBudget{Tokens: 0}, effort.None},
		{"effort high", &latest.ThinkingBudget{Effort: "high"}, effort.High},
		{"effort medium", &latest.ThinkingBudget{Effort: "medium"}, effort.Medium},
		{"token budget treated as none", &latest.ThinkingBudget{Tokens: 4096}, effort.None},
		{"adaptive treated as none", &latest.ThinkingBudget{Effort: "adaptive"}, effort.None},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{ThinkingBudget: tt.budget}
			assert.Equal(t, tt.want, currentThinkingLevel(cfg))
		})
	}
}

func TestModelSupportsThinking(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{} // nil modelsStore: rely on name heuristics only.

	tests := []struct {
		name string
		cfg  latest.ModelConfig
		want bool
	}{
		{"openai reasoning gpt-5", latest.ModelConfig{Provider: "openai", Model: "gpt-5"}, true},
		{"openai o3", latest.ModelConfig{Provider: "openai", Model: "o3"}, true},
		{"openai non-reasoning gpt-4o", latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}, false},
		{"openai gpt-5-chat is non-reasoning", latest.ModelConfig{Provider: "openai", Model: "gpt-5-chat"}, false},
		{"anthropic claude", latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"}, true},
		{"bedrock claude", latest.ModelConfig{Provider: "amazon-bedrock", Model: "us.anthropic.claude-sonnet-4-5"}, true},
		{"gemini 3", latest.ModelConfig{Provider: "google", Model: "gemini-3-pro"}, true},
		{"gemini 2.5", latest.ModelConfig{Provider: "google", Model: "gemini-2.5-pro"}, true},
		{"gemini 2.0 no thinking", latest.ModelConfig{Provider: "google", Model: "gemini-2.0-flash"}, false},
		{"explicit thinking budget overrides heuristic", latest.ModelConfig{Provider: "dmr", Model: "deepseek-r1", ThinkingBudget: &latest.ThinkingBudget{Effort: "medium"}}, true},
		{"disabled thinking budget does not count", latest.ModelConfig{Provider: "dmr", Model: "llama3", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			assert.Equal(t, tt.want, r.modelSupportsThinking(t.Context(), &cfg))
		})
	}
}

func TestCycleAgentThinkingLevel_Errors(t *testing.T) {
	t.Parallel()

	t.Run("nil modelSwitcherCfg is unsupported", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		r := &LocalRuntime{team: team.New(team.WithAgents(root))}

		_, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.ErrorIs(t, err, ErrUnsupported)
	})

	t.Run("agent not found", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		r := &LocalRuntime{
			team:             team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{},
		}

		_, err := r.CycleAgentThinkingLevel(t.Context(), "missing")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent not found")
	})

	t.Run("non-reasoning model is unsupported", func(t *testing.T) {
		t.Parallel()
		model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-4o"})
		root := agent.New("root", "test", agent.WithModel(model))
		r := &LocalRuntime{
			team:             team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{},
		}

		_, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.ErrorIs(t, err, ErrUnsupported)
		assert.False(t, root.HasModelOverride(), "no override should be set when cycling is unsupported")
	})
}

func TestAgentThinkingLabel(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{} // nil modelsStore: name heuristics only.

	tests := []struct {
		name string
		cfg  latest.ModelConfig
		want string
	}{
		{"reasoning model, no budget shows off", latest.ModelConfig{Provider: "openai", Model: "gpt-5"}, "off"},
		{"reasoning model with effort", latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}, "high"},
		{"reasoning model disabled shows off", latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}, "off"},
		{"adaptive budget shows adaptive", latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7", ThinkingBudget: &latest.ThinkingBudget{Effort: "adaptive"}}, "adaptive"},
		{"token budget shows token count", latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 4096}}, "4096"},
		{"non-reasoning model hides line", latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := agent.New("root", "test", agent.WithModel(newConfigProvider(tt.cfg)))
			assert.Equal(t, tt.want, r.agentThinkingLabel(t.Context(), a))
		})
	}
}

func TestCycleAgentThinkingLevel_AdvancesAndOverrides(t *testing.T) {
	t.Parallel()

	model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})
	root := agent.New("root", "test", agent.WithModel(model))
	r := &LocalRuntime{
		team: team.New(team.WithAgents(root)),
		modelSwitcherCfg: &modelcatalog.Config{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
		},
	}

	// gpt-5 has no thinking budget configured → current is None, so the first
	// cycle step lands on Minimal (the OpenAI cycle is none→minimal→…).
	level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
	require.NoError(t, err)
	assert.Equal(t, effort.Minimal, level)
	require.True(t, root.HasModelOverride(), "cycling must install a runtime override")

	override := root.Model(t.Context())
	require.NotNil(t, override)
	budget := override.BaseConfig().ModelConfig.ThinkingBudget
	require.NotNil(t, budget)
	assert.Equal(t, "minimal", budget.Effort)

	// Next step: minimal → low.
	level, err = r.CycleAgentThinkingLevel(t.Context(), "root")
	require.NoError(t, err)
	assert.Equal(t, effort.Low, level)
}

func TestSetAgentThinkingLevel(t *testing.T) {
	t.Parallel()

	newThinkingRuntime := func(cfg latest.ModelConfig, env map[string]string) (*LocalRuntime, *agent.Agent) {
		root := agent.New("root", "test", agent.WithModel(newConfigProvider(cfg)))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(env),
			},
		}
		return r, root
	}

	t.Run("supported level installs override", func(t *testing.T) {
		t.Parallel()
		r, root := newThinkingRuntime(
			latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.High)
		require.NoError(t, err)
		assert.Equal(t, effort.High, level)
		require.True(t, root.HasModelOverride(), "setting a level must install a runtime override")

		override := root.Model(t.Context())
		require.NotNil(t, override)
		budget := override.BaseConfig().ModelConfig.ThinkingBudget
		require.NotNil(t, budget)
		assert.Equal(t, "high", budget.Effort)
	})

	t.Run("unsupported level errors and lists supported levels", func(t *testing.T) {
		t.Parallel()
		r, root := newThinkingRuntime(
			latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		// gpt-5 tops out at high: max must be rejected, not clamped.
		_, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported")
		assert.Contains(t, err.Error(), "high")
		assert.False(t, root.HasModelOverride(), "no override should be set on rejection")
	})

	t.Run("non-reasoning model is unsupported", func(t *testing.T) {
		t.Parallel()
		r, root := newThinkingRuntime(
			latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		_, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.High)
		require.ErrorIs(t, err, ErrUnsupported)
		assert.False(t, root.HasModelOverride())
	})

	t.Run("nil modelSwitcherCfg is unsupported", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		r := &LocalRuntime{team: team.New(team.WithAgents(root))}

		_, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.High)
		require.ErrorIs(t, err, ErrUnsupported)
	})

	t.Run("model-specific top tier is accepted", func(t *testing.T) {
		t.Parallel()
		r, _ := newThinkingRuntime(
			latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7"},
			map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
		)

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
		require.NoError(t, err)
		assert.Equal(t, effort.Max, level)
	})
}

// TestSetAgentThinkingLevel_ConcurrentOverrideBetweenResolutionAndApply is
// the regression test for the single-snapshot fix: applyAgentThinkingLevel
// must resolve capabilities and re-create providers from the SAME
// EffectiveModels snapshot. A second, later snapshot would let a
// concurrent /model change or scoped override land between resolution and
// application, validating the effort against one model but applying it to
// another (see #3731 follow-up).
func TestSetAgentThinkingLevel_ConcurrentOverrideBetweenResolutionAndApply(t *testing.T) {
	t.Parallel()

	t.Run("override swapped mid-flight does not leak into the applied provider", func(t *testing.T) {
		t.Parallel()

		// gpt-5 tops out at high; opus 4.7 accepts max. If applyAgentThinkingLevel
		// re-read EffectiveModels after pick ran, it would recreate the override
		// from opus instead of gpt-5, silently applying "high" to a model that was
		// never validated against it.
		modelA := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})
		modelB := newConfigProvider(latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7"})

		root := agent.New("root", "test", agent.WithModel(modelA))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider: environment.NewMapEnvProvider(map[string]string{
					"OPENAI_API_KEY":    "sk-test",
					"ANTHROPIC_API_KEY": "sk-test",
				}),
			},
		}

		// pick runs after the snapshot is resolved but before providers are
		// re-created; a concurrent /model change landing here is exactly the
		// race the bug exposed.
		level, err := r.applyAgentThinkingLevel(t.Context(), "root", func(supported []effort.Level, _ effort.Level) (effort.Level, error) {
			require.NotContains(t, supported, effort.Max, "resolution must see model A (gpt-5), which tops out at high")
			root.SetModelOverride(modelB)
			return effort.High, nil
		})
		require.NoError(t, err)
		assert.Equal(t, effort.High, level)

		override := root.Model(t.Context())
		require.NotNil(t, override)
		assert.Equal(t, "openai", override.BaseConfig().ModelConfig.Provider,
			"the applied provider must be re-created from the model validated against (A), not the one swapped in mid-flight (B)")
		budget := override.BaseConfig().ModelConfig.ThinkingBudget
		require.NotNil(t, budget)
		assert.Equal(t, "high", budget.Effort)
	})

	t.Run("override cleared mid-flight does not install an empty override", func(t *testing.T) {
		t.Parallel()

		// No configured models: the agent's only model comes from the runtime
		// override, so clearing it mid-flight reproduces the empty second-snapshot
		// case the bug allowed through.
		modelA := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})
		root := agent.New("root", "test")
		root.SetModelOverride(modelA)

		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
			},
		}

		level, err := r.applyAgentThinkingLevel(t.Context(), "root", func(_ []effort.Level, _ effort.Level) (effort.Level, error) {
			// Simulate a concurrent override reset (e.g. /model default) landing
			// between resolution and provider recreation.
			root.SetModelOverride()
			return effort.High, nil
		})
		require.NoError(t, err)
		assert.Equal(t, effort.High, level)

		override := root.Model(t.Context())
		require.NotNil(t, override, "must not silently install an empty override while reporting success")
		assert.Equal(t, "openai", override.BaseConfig().ModelConfig.Provider)
	})
}

func TestCurrentAgentThinkingLevels(t *testing.T) {
	t.Parallel()

	newThinkingRuntimeWithRouter := func(cfg latest.ModelConfig, env map[string]string) *LocalRuntime {
		root := agent.New("root", "test", agent.WithModel(newConfigProvider(cfg)))
		tm := team.New(team.WithAgents(root))
		return &LocalRuntime{
			team:   tm,
			agents: newAgentRouter(tm, "root"),
			modelSwitcherCfg: &modelcatalog.Config{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(env),
			},
		}
	}

	t.Run("reasoning model returns its supported levels", func(t *testing.T) {
		t.Parallel()
		r := newThinkingRuntimeWithRouter(
			latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		levels := r.CurrentAgentThinkingLevels(t.Context())
		assert.NotEmpty(t, levels)
		assert.Contains(t, levels, effort.High)
		assert.NotContains(t, levels, effort.Max, "gpt-5 tops out below max")
	})

	t.Run("non-reasoning model is nil", func(t *testing.T) {
		t.Parallel()
		r := newThinkingRuntimeWithRouter(
			latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		assert.Nil(t, r.CurrentAgentThinkingLevels(t.Context()))
	})

	t.Run("nil modelSwitcherCfg is nil", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root")}

		assert.Nil(t, r.CurrentAgentThinkingLevels(t.Context()))
	})

	t.Run("matches the levels SetAgentThinkingLevel validates against", func(t *testing.T) {
		t.Parallel()
		r := newThinkingRuntimeWithRouter(
			latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7"},
			map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
		)

		levels := r.CurrentAgentThinkingLevels(t.Context())
		require.Contains(t, levels, effort.Max, "opus 4.7 top tier")

		// Every level CurrentAgentThinkingLevels reports must be one
		// SetAgentThinkingLevel actually accepts -- the single-source-of-truth
		// guarantee #3731 depends on.
		for _, level := range levels {
			_, err := r.SetAgentThinkingLevel(t.Context(), "root", level)
			assert.NoError(t, err, "level %q reported as supported but rejected", level)
		}
	})
}

func newHarnessThinkingRuntime(cfg *latest.HarnessConfig) (*LocalRuntime, *agent.Agent) {
	root := agent.New("root", "test", agent.WithHarness(cfg))
	tm := team.New(team.WithAgents(root))
	return &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root"), modelSwitcherCfg: &modelcatalog.Config{}}, root
}

// A Claude Code harness agent has no models, but its effort reaches the CLI's
// --effort flag: /effort and Shift+Tab switch it like a model's level.
func TestHarnessAgentThinkingLevels(t *testing.T) {
	t.Parallel()

	allEfforts := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}

	t.Run("unset effort shows adaptive and cycling starts at low", func(t *testing.T) {
		t.Parallel()
		r, root := newHarnessThinkingRuntime(&latest.HarnessConfig{Type: "claude-code"})

		assert.Equal(t, allEfforts, r.CurrentAgentThinkingLevels(t.Context()))
		assert.Equal(t, "adaptive", r.agentThinkingLabel(t.Context(), root))

		level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.NoError(t, err)
		assert.Equal(t, effort.Low, level)
		level, err = r.CycleAgentThinkingLevel(t.Context(), "root")
		require.NoError(t, err)
		assert.Equal(t, effort.Medium, level)
		assert.Equal(t, "medium", root.Harness().Effort)
		assert.Equal(t, "medium", r.agentThinkingLabel(t.Context(), root))
	})

	t.Run("configured effort cycles from its level and wraps", func(t *testing.T) {
		t.Parallel()
		r, root := newHarnessThinkingRuntime(&latest.HarnessConfig{Type: "command-code", Effort: "max"})
		assert.Equal(t, "max", r.agentThinkingLabel(t.Context(), root))

		level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.NoError(t, err)
		assert.Equal(t, effort.Low, level)
	})

	t.Run("set accepts the CLI efforts only", func(t *testing.T) {
		t.Parallel()
		r, root := newHarnessThinkingRuntime(&latest.HarnessConfig{Type: "claude-code", Model: "opus", Effort: "low"})

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.XHigh)
		require.NoError(t, err)
		assert.Equal(t, effort.XHigh, level)
		assert.Equal(t, latest.HarnessConfig{Type: "claude-code", Model: "opus", Effort: "xhigh"}, *root.Harness())

		_, err = r.SetAgentThinkingLevel(t.Context(), "root", effort.None)
		require.ErrorContains(t, err, "not supported")
		assert.Equal(t, "xhigh", root.Harness().Effort)
	})

	t.Run("harness without an effort flag is unsupported", func(t *testing.T) {
		t.Parallel()
		r, root := newHarnessThinkingRuntime(&latest.HarnessConfig{Type: "codex"})

		_, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.ErrorIs(t, err, ErrUnsupported)
		assert.Nil(t, r.CurrentAgentThinkingLevels(t.Context()))
		assert.Empty(t, r.agentThinkingLabel(t.Context(), root))
	})
}

// TestSetAgentThinkingLevel_NoneSurvivesOnGPT56 is the regression test for
// the gpt-5.6+ "none" gating: cycling an agent's model to None on a model
// with a real API-level none effort must produce a provider whose config
// still carries an explicit ThinkingBudget{Effort: "none"} (not nil), so the
// value actually reaches the OpenAI client instead of silently reverting to
// the model's default effort. Older OpenAI models (no none effort) keep the
// existing nil-normalize behavior.
func TestSetAgentThinkingLevel_NoneSurvivesOnGPT56(t *testing.T) {
	t.Parallel()

	t.Run("gpt-5.6 preserves explicit none", func(t *testing.T) {
		t.Parallel()
		model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}})
		root := agent.New("root", "test", agent.WithModel(model))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
			},
		}

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.None)
		require.NoError(t, err)
		assert.Equal(t, effort.None, level)

		override := root.Model(t.Context())
		require.NotNil(t, override)
		budget := override.BaseConfig().ModelConfig.ThinkingBudget
		require.NotNil(t, budget, "gpt-5.6 has a real API none effort; the budget must survive provider construction")
		assert.Equal(t, "none", budget.Effort)
	})

	t.Run("gpt-5.2 normalizes none to nil", func(t *testing.T) {
		t.Parallel()
		model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5.2", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}})
		root := agent.New("root", "test", agent.WithModel(model))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &modelcatalog.Config{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
			},
		}

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.None)
		require.NoError(t, err)
		assert.Equal(t, effort.None, level)

		override := root.Model(t.Context())
		require.NotNil(t, override)
		assert.Nil(t, override.BaseConfig().ModelConfig.ThinkingBudget, "gpt-5.2 has no API none effort; it must keep the nil-normalize behavior")
	})
}

// TestCycleAgentThinkingLevel_PerModelTopTier verifies that cycling only
// offers the top effort tiers to the Claude models whose API accepts them.
func TestCycleAgentThinkingLevel_PerModelTopTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modelID   string
		wantCycle []effort.Level
	}{
		{
			name:      "sonnet 4.5 never reaches xhigh or max",
			modelID:   "claude-sonnet-4-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.None},
		},
		{
			name:      "opus 4.5 never reaches xhigh or max",
			modelID:   "claude-opus-4-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.None},
		},
		{
			name:      "sonnet 4.6 reaches max but not xhigh",
			modelID:   "claude-sonnet-4-6",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max, effort.None},
		},
		{
			name:      "opus 4.6 reaches max but not xhigh",
			modelID:   "claude-opus-4-6",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max, effort.None},
		},
		{
			name:      "opus 4.7 reaches both xhigh and max",
			modelID:   "claude-opus-4-7",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "opus 4.8 reaches both xhigh and max",
			modelID:   "claude-opus-4-8",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "fable 5 reaches both xhigh and max",
			modelID:   "claude-fable-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "mythos 5 reaches both xhigh and max",
			modelID:   "claude-mythos-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "mythos preview reaches max but not xhigh",
			modelID:   "claude-mythos-preview",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max, effort.None},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := newConfigProvider(latest.ModelConfig{Provider: "anthropic", Model: tt.modelID})
			root := agent.New("root", "test", agent.WithModel(model))
			r := &LocalRuntime{
				team: team.New(team.WithAgents(root)),
				modelSwitcherCfg: &modelcatalog.Config{
					ProviderRegistry: testProviderRegistry(),
					EnvProvider:      environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
				},
			}

			// Walk one full cycle starting from None and record every level.
			var got []effort.Level
			for range tt.wantCycle {
				level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
				require.NoError(t, err)
				got = append(got, level)
			}
			assert.Equal(t, tt.wantCycle, got)
		})
	}
}

// TestCycleAgentThinkingLevel_VercelQualifiedGPT56 is the runtime regression
// test for the second-review Vercel finding: the exact default from
// pkg/config/auto.go ("vercel" provider, "openai/gpt-5.6-sol" model) must
// cycle through the full gpt-5.6 ladder — including xhigh and max — even
// though the provider name itself ("vercel") is not "openai". Minimal is
// skipped: gpt-5.6 drops it.
func TestCycleAgentThinkingLevel_VercelQualifiedGPT56(t *testing.T) {
	t.Parallel()

	model := newConfigProvider(latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol"})
	root := agent.New("root", "test", agent.WithModel(model))
	r := &LocalRuntime{
		team: team.New(team.WithAgents(root)),
		modelSwitcherCfg: &modelcatalog.Config{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"AI_GATEWAY_API_KEY": "vck-test"}),
		},
	}

	wantCycle := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None}
	var got []effort.Level
	for range wantCycle {
		level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.NoError(t, err)
		got = append(got, level)
	}
	assert.Equal(t, wantCycle, got)
}

// TestSetAgentThinkingLevel_VercelQualifiedGPT56Max verifies that "max" is
// accepted (not rejected as unsupported) for the exact Vercel default model,
// and that the resulting override still carries the "vercel" provider and
// the "openai/" qualified model id.
func TestSetAgentThinkingLevel_VercelQualifiedGPT56Max(t *testing.T) {
	t.Parallel()

	model := newConfigProvider(latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol"})
	root := agent.New("root", "test", agent.WithModel(model))
	r := &LocalRuntime{
		team: team.New(team.WithAgents(root)),
		modelSwitcherCfg: &modelcatalog.Config{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"AI_GATEWAY_API_KEY": "vck-test"}),
		},
	}

	level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
	require.NoError(t, err)
	assert.Equal(t, effort.Max, level)

	override := root.Model(t.Context())
	require.NotNil(t, override)
	cfg := override.BaseConfig().ModelConfig
	assert.Equal(t, "vercel", cfg.Provider)
	assert.Equal(t, "openai/gpt-5.6-sol", cfg.Model)
	require.NotNil(t, cfg.ThinkingBudget)
	assert.Equal(t, "max", cfg.ThinkingBudget.Effort)
}

func TestIsInlineAlloySpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		modelRef string
		want     bool
	}{
		{
			name:     "single inline model",
			modelRef: "openai/gpt-4o",
			want:     false,
		},
		{
			name:     "two inline models",
			modelRef: "openai/gpt-4o,anthropic/claude-sonnet-4-0",
			want:     true,
		},
		{
			name:     "three inline models",
			modelRef: "openai/gpt-4o,anthropic/claude-sonnet-4-0,google/gemini-2.0-flash",
			want:     true,
		},
		{
			name:     "with spaces",
			modelRef: "openai/gpt-4o, anthropic/claude-sonnet-4-0",
			want:     true,
		},
		{
			name:     "named model (no slash)",
			modelRef: "my_fast_model",
			want:     false,
		},
		{
			name:     "comma separated named models (not inline alloy)",
			modelRef: "fast_model,smart_model",
			want:     false,
		},
		{
			name:     "mixed named and inline",
			modelRef: "fast_model,openai/gpt-4o",
			want:     false, // "fast_model" doesn't contain "/" so it's not an inline alloy
		},
		{
			name:     "empty string",
			modelRef: "",
			want:     false,
		},
		{
			name:     "just commas",
			modelRef: ",,",
			want:     false, // No valid parts after trimming
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isInlineAlloySpec(tt.modelRef)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveModelRef_RejectsAlloyConfig(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{
		modelSwitcherCfg: &modelcatalog.Config{
			ProviderRegistry: testProviderRegistry(),
			Models: map[string]latest.ModelConfig{
				// Alloy config: no provider, comma-separated models
				"alloy_model": {Model: "openai/gpt-4o,anthropic/claude-sonnet-4-0"},
			},
		},
	}

	_, err := r.resolveModelRef(t.Context(), "alloy_model")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alloy")
}

func TestResolveModelRef_NilConfig(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{}

	_, err := r.resolveModelRef(t.Context(), "openai/gpt-4o")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestResolveModelRef_InvalidFormat(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{
		modelSwitcherCfg: &modelcatalog.Config{
			ProviderRegistry: testProviderRegistry(),
			Models:           map[string]latest.ModelConfig{},
		},
	}

	tests := []struct {
		name     string
		modelRef string
	}{
		{"no slash", "invalid"},
		{"empty provider", "/model"},
		{"empty model", "provider/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.resolveModelRef(t.Context(), tt.modelRef)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid model reference")
		})
	}
}

func TestDecorateModelChoices(t *testing.T) {
	t.Parallel()

	t.Run("default marked current when no override is set", func(t *testing.T) {
		t.Parallel()
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{
				{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
				{Name: "other", Ref: "openai/gpt-4o"},
			},
			"",
			nil,
		)
		require.Len(t, got, 2)
		assert.True(t, got[0].IsCurrent, "the IsDefault model must be marked IsCurrent when no override is set")
		assert.False(t, got[1].IsCurrent)
	})

	t.Run("override matching a known choice marks it current", func(t *testing.T) {
		t.Parallel()
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{
				{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
				{Name: "other", Ref: "openai/gpt-4o"},
			},
			"openai/gpt-4o",
			nil,
		)
		require.Len(t, got, 2)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when an override is active")
		assert.True(t, got[1].IsCurrent)
	})

	t.Run("synthesizes choice for inline override not in list", func(t *testing.T) {
		t.Parallel()
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"anthropic/claude-sonnet-4-0",
			nil,
		)
		require.Len(t, got, 2)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[1].Ref)
		assert.Equal(t, "anthropic", got[1].Provider)
		assert.Equal(t, "claude-sonnet-4-0", got[1].Model)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	t.Run("appends custom refs from session history", func(t *testing.T) {
		t.Parallel()
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"},
		)
		require.Len(t, got, 3)
		assert.Equal(t, "openai/gpt-4o-mini", got[0].Ref)
		assert.Equal(t, "openai/gpt-4o", got[1].Ref)
		assert.True(t, got[1].IsCustom)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[2].Ref)
		assert.True(t, got[2].IsCustom)
	})

	t.Run("does not duplicate custom ref already in list", func(t *testing.T) {
		t.Parallel()
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{{Name: "default", Ref: "openai/gpt-4o", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 1)
		assert.Equal(t, "openai/gpt-4o", got[0].Ref)
	})

	t.Run("non-provider override does not synthesize a fabricated choice", func(t *testing.T) {
		t.Parallel()
		// "my_model" is a config key (no slash); when not in the runtime's
		// list we should NOT fabricate a choice for it because we have no
		// provider/model breakdown to display.
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"my_model",
			nil,
		)
		require.Len(t, got, 1)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when override is unknown")
	})

	t.Run("custom ref matching the active override is marked current", func(t *testing.T) {
		t.Parallel()
		got := modelcatalog.DecorateModelChoices(
			[]modelcatalog.ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"openai/gpt-4o",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 2)
		assert.False(t, got[0].IsCurrent)
		assert.Equal(t, "openai/gpt-4o", got[1].Ref)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	// AvailableModels implementations may return a slice backed by an
	// internal cache; mutating its IsCurrent flag in place would leak
	// state across sessions. The function must therefore never modify
	// the input slice or its underlying array.
	t.Run("does not mutate the input slice", func(t *testing.T) {
		t.Parallel()
		input := []modelcatalog.ModelChoice{
			{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
			{Name: "other", Ref: "openai/gpt-4o"},
		}
		orig := slices.Clone(input)

		_ = modelcatalog.DecorateModelChoices(input, "openai/gpt-4o", []string{"anthropic/claude-sonnet-4-0"})

		assert.Equal(t, orig, input, "DecorateModelChoices must not modify the input slice")
	})
}

func TestAgentThinkingConfigurationIsLocalAndConservative(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cfg      latest.ModelConfig
		multiple bool
		want     bool
	}{
		{name: "explicit", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}, want: true},
		{name: "unknown", cfg: latest.ModelConfig{Provider: "custom", Model: "unknown"}},
		{name: "budget-only recognition", cfg: latest.ModelConfig{Provider: "openai", Model: "custom-reasoner", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}},
		{name: "router", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5", Routing: []latest.RoutingRule{{}}}},
		{name: "alloy", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5"}, multiple: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := []provider.Provider{newConfigProvider(tc.cfg)}
			if tc.multiple {
				models = append(models, newConfigProvider(tc.cfg))
			}
			a := agent.New("root", "test")
			for _, model := range models {
				agent.WithModel(model)(a)
			}
			r := &LocalRuntime{team: team.New(team.WithAgents(a)), modelSwitcherCfg: &modelcatalog.Config{}}
			levels, current := r.AgentThinkingConfiguration("root")
			assert.Equal(t, tc.want, len(levels) > 0)
			if tc.want {
				assert.Equal(t, effort.High, current)
			}
		})
	}
}
