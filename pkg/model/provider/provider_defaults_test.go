package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestMergeFromProviderConfig_Fields exercises every individual field merge
// branch in mergeFromProviderConfig with a single table-driven test.
func TestMergeFromProviderConfig_Fields(t *testing.T) {
	t.Parallel()

	floatPtr := func(v float64) *float64 { return &v }
	int64Ptr := func(v int64) *int64 { return &v }
	boolPtr := func(v bool) *bool { return &v }

	tests := []struct {
		name   string
		dst    *latest.ModelConfig
		src    latest.ProviderConfig
		assert func(t *testing.T, got *latest.ModelConfig)
	}{
		{
			name: "src.Provider overrides dst.Provider",
			dst:  &latest.ModelConfig{Provider: "my_alias"},
			src:  latest.ProviderConfig{Provider: "anthropic"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "anthropic", got.Provider)
			},
		},
		{
			name: "empty src.Provider keeps dst.Provider",
			dst:  &latest.ModelConfig{Provider: "openai"},
			src:  latest.ProviderConfig{},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "openai", got.Provider)
			},
		},
		{
			name: "BaseURL filled when empty",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{BaseURL: "https://api.example.com"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "https://api.example.com", got.BaseURL)
			},
		},
		{
			name: "BaseURL preserved when set",
			dst:  &latest.ModelConfig{BaseURL: "https://override"},
			src:  latest.ProviderConfig{BaseURL: "https://default"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "https://override", got.BaseURL)
			},
		},
		{
			name: "TokenKey filled when empty",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{TokenKey: "MY_KEY"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "MY_KEY", got.TokenKey)
			},
		},
		{
			name: "TokenKey preserved when set",
			dst:  &latest.ModelConfig{TokenKey: "OVERRIDE"},
			src:  latest.ProviderConfig{TokenKey: "DEFAULT"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "OVERRIDE", got.TokenKey)
			},
		},
		{
			name: "Temperature filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{Temperature: floatPtr(0.5)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.Temperature)
				assert.InDelta(t, 0.5, *got.Temperature, 1e-9)
			},
		},
		{
			name: "Temperature preserved when set",
			dst:  &latest.ModelConfig{Temperature: floatPtr(0.9)},
			src:  latest.ProviderConfig{Temperature: floatPtr(0.1)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.Temperature)
				assert.InDelta(t, 0.9, *got.Temperature, 1e-9)
			},
		},
		{
			name: "MaxTokens filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{MaxTokens: int64Ptr(1024)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.MaxTokens)
				assert.Equal(t, int64(1024), *got.MaxTokens)
			},
		},
		{
			name: "TopP filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{TopP: floatPtr(0.95)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.TopP)
				assert.InDelta(t, 0.95, *got.TopP, 1e-9)
			},
		},
		{
			name: "FrequencyPenalty filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{FrequencyPenalty: floatPtr(0.2)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.FrequencyPenalty)
				assert.InDelta(t, 0.2, *got.FrequencyPenalty, 1e-9)
			},
		},
		{
			name: "PresencePenalty filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{PresencePenalty: floatPtr(0.3)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.PresencePenalty)
				assert.InDelta(t, 0.3, *got.PresencePenalty, 1e-9)
			},
		},
		{
			name: "ParallelToolCalls filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{ParallelToolCalls: boolPtr(true)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.ParallelToolCalls)
				assert.True(t, *got.ParallelToolCalls)
			},
		},
		{
			name: "TrackUsage filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{TrackUsage: boolPtr(true)},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.TrackUsage)
				assert.True(t, *got.TrackUsage)
			},
		},
		{
			name: "ThinkingBudget filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{ThinkingBudget: &latest.ThinkingBudget{Tokens: 4096}},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.ThinkingBudget)
				assert.Equal(t, 4096, got.ThinkingBudget.Tokens)
			},
		},
		{
			name: "TaskBudget filled when nil",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{TaskBudget: &latest.TaskBudget{Total: 200000}},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.TaskBudget)
				assert.Equal(t, 200000, got.TaskBudget.Total)
			},
		},
		{
			name: "ProviderOpts merged (model opts take precedence)",
			dst:  &latest.ModelConfig{ProviderOpts: map[string]any{"shared": "from_model"}},
			src:  latest.ProviderConfig{ProviderOpts: map[string]any{"shared": "from_provider", "extra": 42}},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "from_model", got.ProviderOpts["shared"])
				assert.Equal(t, 42, got.ProviderOpts["extra"])
			},
		},
		{
			name: "ProviderOpts created lazily when nil on dst",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{ProviderOpts: map[string]any{"k": "v"}},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.ProviderOpts)
				assert.Equal(t, "v", got.ProviderOpts["k"])
			},
		},
		{
			name: "UnloadAPI plumbed into ProviderOpts.unload_api",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{UnloadAPI: "/engines/_unload"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				require.NotNil(t, got.ProviderOpts)
				assert.Equal(t, "/engines/_unload", got.ProviderOpts["unload_api"])
			},
		},
		{
			name: "UnloadAPI does not overwrite an existing model-level unload_api",
			dst:  &latest.ModelConfig{ProviderOpts: map[string]any{"unload_api": "/custom"}},
			src:  latest.ProviderConfig{UnloadAPI: "/engines/_unload"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "/custom", got.ProviderOpts["unload_api"])
			},
		},
		{
			name: "empty UnloadAPI doesn't pollute ProviderOpts",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				if got.ProviderOpts != nil {
					_, has := got.ProviderOpts["unload_api"]
					assert.False(t, has)
				}
			},
		},
		{
			name: "explicit APIType wins over OpenAI-compatible default",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{APIType: "openai_responses"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "openai_responses", got.ProviderOpts["api_type"])
			},
		},
		{
			name: "OpenAI-compatible provider defaults api_type to openai_chatcompletions",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{Provider: "openai"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "openai_chatcompletions", got.ProviderOpts["api_type"])
			},
		},
		{
			name: "non-OpenAI provider does not get a default api_type",
			dst:  &latest.ModelConfig{},
			src:  latest.ProviderConfig{Provider: "anthropic"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				if got.ProviderOpts != nil {
					_, has := got.ProviderOpts["api_type"]
					assert.False(t, has)
				}
			},
		},
		{
			name: "model-set api_type wins over provider-set api_type",
			dst:  &latest.ModelConfig{ProviderOpts: map[string]any{"api_type": "openai_responses"}},
			src:  latest.ProviderConfig{APIType: "openai_chatcompletions"},
			assert: func(t *testing.T, got *latest.ModelConfig) {
				t.Helper()
				assert.Equal(t, "openai_responses", got.ProviderOpts["api_type"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mergeFromProviderConfig(tt.dst, tt.src)
			tt.assert(t, tt.dst)
		})
	}
}

func TestApplyAliasFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		dst          *latest.ModelConfig
		alias        Alias
		wantBaseURL  string
		wantTokenKey string
	}{
		{
			name:         "fills empty BaseURL and TokenKey",
			dst:          &latest.ModelConfig{},
			alias:        Alias{BaseURL: "https://api.example.com", TokenEnvVar: "X_KEY"},
			wantBaseURL:  "https://api.example.com",
			wantTokenKey: "X_KEY",
		},
		{
			name:         "preserves explicit BaseURL",
			dst:          &latest.ModelConfig{BaseURL: "https://override"},
			alias:        Alias{BaseURL: "https://default", TokenEnvVar: "X_KEY"},
			wantBaseURL:  "https://override",
			wantTokenKey: "X_KEY",
		},
		{
			name:         "preserves explicit TokenKey",
			dst:          &latest.ModelConfig{TokenKey: "OVERRIDE"},
			alias:        Alias{BaseURL: "https://default", TokenEnvVar: "DEFAULT"},
			wantBaseURL:  "https://default",
			wantTokenKey: "OVERRIDE",
		},
		{
			name:         "alias with only BaseURL (e.g. ollama)",
			dst:          &latest.ModelConfig{},
			alias:        Alias{BaseURL: "http://localhost:11434/v1"},
			wantBaseURL:  "http://localhost:11434/v1",
			wantTokenKey: "",
		},
		{
			name:         "alias with only TokenEnvVar (e.g. azure)",
			dst:          &latest.ModelConfig{},
			alias:        Alias{TokenEnvVar: "AZURE_API_KEY"},
			wantBaseURL:  "",
			wantTokenKey: "AZURE_API_KEY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			applyAliasFallbacks(tt.dst, tt.alias)
			assert.Equal(t, tt.wantBaseURL, tt.dst.BaseURL)
			assert.Equal(t, tt.wantTokenKey, tt.dst.TokenKey)
		})
	}
}

func TestSetIfNil(t *testing.T) {
	t.Parallel()

	t.Run("fills nil dst from non-nil src", func(t *testing.T) {
		t.Parallel()
		var dst *int
		v := 42
		setIfNil(&dst, &v)
		require.NotNil(t, dst)
		assert.Equal(t, 42, *dst)
	})

	t.Run("preserves non-nil dst", func(t *testing.T) {
		t.Parallel()
		original := 1
		dst := &original
		v := 42
		setIfNil(&dst, &v)
		assert.Equal(t, 1, *dst)
	})

	t.Run("nil src leaves dst nil", func(t *testing.T) {
		t.Parallel()
		var dst *string
		setIfNil[string](&dst, nil)
		assert.Nil(t, dst)
	})

	t.Run("nil src leaves dst untouched", func(t *testing.T) {
		t.Parallel()
		original := "hi"
		dst := &original
		setIfNil[string](&dst, nil)
		assert.Equal(t, "hi", *dst)
	})
}

func TestSetProviderOptIfAbsent(t *testing.T) {
	t.Parallel()

	t.Run("creates map lazily and sets value", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{}
		setProviderOptIfAbsent(cfg, "k", "v")
		require.NotNil(t, cfg.ProviderOpts)
		assert.Equal(t, "v", cfg.ProviderOpts["k"])
	})

	t.Run("preserves existing key", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{ProviderOpts: map[string]any{"k": "old"}}
		setProviderOptIfAbsent(cfg, "k", "new")
		assert.Equal(t, "old", cfg.ProviderOpts["k"])
	})

	t.Run("adds new key alongside existing", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{ProviderOpts: map[string]any{"a": 1}}
		setProviderOptIfAbsent(cfg, "b", 2)
		assert.Equal(t, 1, cfg.ProviderOpts["a"])
		assert.Equal(t, 2, cfg.ProviderOpts["b"])
	})
}

// TestApplyProviderDefaults_AliasFallback covers the alias-only branch
// (no custom-provider entry) which exercises applyAliasFallbacks indirectly.
func TestApplyProviderDefaults_AliasFallback(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{Provider: "mistral", Model: "mistral-large-latest"}
	got := applyProviderDefaults(cfg, nil)

	assert.Equal(t, "https://api.mistral.ai/v1", got.BaseURL)
	assert.Equal(t, "MISTRAL_API_KEY", got.TokenKey)
	// Original must not be mutated.
	assert.Empty(t, cfg.BaseURL)
	assert.Empty(t, cfg.TokenKey)
}
