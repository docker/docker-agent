package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// captureFactory records the config the factory receives so tests can assert
// the value-bearing fields were expanded before the provider was built.
func captureFactory(into **latest.ModelConfig) providerFactory {
	return func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
		*into = cfg
		return &fakeProvider{}, nil
	}
}

// TestCreateDirectProvider_ExpandsModelEnv covers issue #2261: ${env.X} in a
// model config's model and base_url fields must be substituted against the
// runtime environment before the provider is built, instead of being passed
// through verbatim.
func TestCreateDirectProvider_ExpandsModelEnv(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{
		"NEMOTRON3_MODEL": "huggingface.co/unsloth/nemotron-3-nano-30b-a3b-gguf:Q3_K_M",
		"DMR_BASE_URL":    "http://localhost:12434/engines/v1",
	})

	var got *latest.ModelConfig
	r := NewRegistry(map[string]providerFactory{"dmr": captureFactory(&got)})

	cfg := &latest.ModelConfig{
		Provider: "dmr",
		Model:    "${env.NEMOTRON3_MODEL}",
		BaseURL:  "${env.DMR_BASE_URL}",
	}

	_, err := r.createDirectProvider(t.Context(), cfg, env)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "huggingface.co/unsloth/nemotron-3-nano-30b-a3b-gguf:Q3_K_M", got.Model)
	assert.Equal(t, "http://localhost:12434/engines/v1", got.BaseURL)

	// The caller's config must be left untouched: expansion happens on the
	// clone applyProviderDefaults returns, not the shared models map entry.
	assert.Equal(t, "${env.NEMOTRON3_MODEL}", cfg.Model)
	assert.Equal(t, "${env.DMR_BASE_URL}", cfg.BaseURL)
}

// TestCreateDirectProvider_ExpandsBareAndAliasForms verifies the shell-style
// ${X} alias is accepted alongside the JS-template ${env.X} form, matching the
// other env-expanded config fields.
func TestCreateDirectProvider_ExpandsBareAndAliasForms(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{"MY_MODEL": "gpt-4o"})

	var got *latest.ModelConfig
	r := NewRegistry(map[string]providerFactory{"openai": captureFactory(&got)})

	cfg := &latest.ModelConfig{Provider: "openai", Model: "${MY_MODEL}"}
	_, err := r.createDirectProvider(t.Context(), cfg, env)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "gpt-4o", got.Model)
}

// TestCreateDirectProvider_MissingEnvVarErrors verifies an unset variable
// surfaces as a clear provider-creation error rather than dialing with an
// empty model id.
func TestCreateDirectProvider_MissingEnvVarErrors(t *testing.T) {
	t.Parallel()

	r := NewRegistry(map[string]providerFactory{"dmr": tagFactory("dmr")})
	cfg := &latest.ModelConfig{Provider: "dmr", Model: "${env.NOT_SET_MODEL}"}

	_, err := r.createDirectProvider(t.Context(), cfg, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOT_SET_MODEL")
}

// TestCreateDirectProvider_PlainModelUnchanged verifies configs without any
// reference are left exactly as-is (no spurious expansion or error).
func TestCreateDirectProvider_PlainModelUnchanged(t *testing.T) {
	t.Parallel()

	var got *latest.ModelConfig
	r := NewRegistry(map[string]providerFactory{"openai": captureFactory(&got)})

	cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1"}
	_, err := r.createDirectProvider(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "gpt-4o", got.Model)
	assert.Equal(t, "https://api.openai.com/v1", got.BaseURL)
}

func TestCreateDirectProvider_ExpandedDisabledThinking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, model string
		preserve        bool
	}{
		{"anthropic", "claude-haiku-5-5", true},
		{"anthropic", "claude-haiku-4-5", false},
		{"openai", "gpt-5.6-sol", true},
		{"openai", "claude-haiku-5-5", false},
		{"vercel", "anthropic/claude-haiku-5-5", false},
	} {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			t.Parallel()
			var got *latest.ModelConfig
			r := NewRegistry(map[string]providerFactory{tc.provider: captureFactory(&got), "openai": captureFactory(&got)})
			cfg := &latest.ModelConfig{Provider: tc.provider, Model: "${env.MODEL}", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}
			_, err := r.New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"MODEL": tc.model}))
			require.NoError(t, err)
			if tc.preserve {
				require.NotNil(t, got.ThinkingBudget)
				assert.Equal(t, "none", got.ThinkingBudget.Effort)
			} else {
				assert.Nil(t, got.ThinkingBudget)
			}
			assert.Equal(t, "${env.MODEL}", cfg.Model)
			assert.Equal(t, "none", cfg.ThinkingBudget.Effort)
		})
	}
}
