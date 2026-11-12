package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// Claude 5.x models accept server-side fallback, except that Haiku 5.5 has
// none: a list is a 400 there, and default has no effect.
func TestNewClient_Claude5Fallbacks(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "k"})
	for name, tc := range map[string]struct {
		model     string
		fallbacks any
		wantErr   string
	}{
		"fable 5.1 default": {model: "claude-fable-5-1", fallbacks: "default"},
		"fable 5.1 list":    {model: "claude-fable-5-1", fallbacks: []any{"claude-opus-5-5"}},
		"legacy to 5.x":     {model: "claude-opus-4-8", fallbacks: []any{"claude-sonnet-5-5"}},
		"sonnet 5.5 to 5":   {model: "claude-sonnet-5-5", fallbacks: []any{"claude-sonnet-5"}},
		"haiku 5.5 default": {model: "claude-haiku-5-5", fallbacks: "default"},
		"haiku 5.5 list":    {model: "claude-haiku-5-5", fallbacks: []any{"claude-sonnet-4-6"}, wantErr: "no server-side fallback"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: "anthropic", Model: tc.model, ProviderOpts: map[string]any{"fallbacks": tc.fallbacks}}
			_, err := NewClient(t.Context(), cfg, env)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestParseFallbacks(t *testing.T) {
	t.Parallel()

	valid := map[string]struct {
		value any
		want  fallbackSpec
	}{
		"absent":       {},
		"empty list":   {value: []any{}},
		"default":      {value: "default", want: fallbackSpec{Default: true}},
		"one model":    {value: []any{"claude-opus-4-8"}, want: fallbackSpec{Models: []string{"claude-opus-4-8"}}},
		"three models": {value: []any{"a", "b", "c"}, want: fallbackSpec{Models: []string{"a", "b", "c"}}},
		"string slice": {value: []string{"a"}, want: fallbackSpec{Models: []string{"a"}}},
	}
	for name, tc := range valid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := map[string]any{}
			if tc.value != nil {
				opts["fallbacks"] = tc.value
			}
			got, err := parseFallbacks(opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	invalid := map[string]any{
		"other string": "claude-opus-4-8",
		"four models":  []any{"a", "b", "c", "d"},
		"duplicate":    []any{"a", "a"},
		"empty entry":  []any{"a", ""},
		"non-string":   []any{"a", 1},
		"object entry": []any{map[string]any{"model": "a"}},
		"wrong type":   42,
	}
	for name, value := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			t.Parallel()
			_, err := parseFallbacks(map[string]any{"fallbacks": value})
			require.ErrorContains(t, err, "fallbacks")
		})
	}
}

func TestValidateFallbackSpec(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		model     string
		fallbacks any
		wantErr   string
	}{
		"list":                   {model: "claude-opus-4-8", fallbacks: []any{"claude-sonnet-4-6"}},
		"default":                {model: "claude-opus-4-8", fallbacks: "default"},
		"entry equals primary":   {model: "claude-opus-4-8", fallbacks: []any{"claude-opus-4-8"}, wantErr: "same as the model"},
		"malformed":              {model: "claude-opus-4-8", fallbacks: "nope", wantErr: "fallbacks"},
		"haiku 5.5 list":         {model: "claude-haiku-5-5", fallbacks: []any{"claude-sonnet-4-6"}, wantErr: "no server-side fallback"},
		"haiku 5.5 default only": {model: "claude-haiku-5-5", fallbacks: "default"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateFallbackSpec(&latest.ModelConfig{Provider: "anthropic", Model: tc.model, ProviderOpts: map[string]any{"fallbacks": tc.fallbacks}})
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestNewClient_RejectsMalformedFallbacks(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-8", ProviderOpts: map[string]any{"fallbacks": "claude-sonnet-4-6"}}
	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "k"}))
	require.ErrorContains(t, err, `fallbacks must be "default" or a list`)
}

func TestNewClient_FallbacksOnlyOnClaudeAPI(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "k"})
	for name, tc := range map[string]struct {
		baseURL string
		opts    []options.Opt
		wantErr string
	}{
		"claude api":          {},
		"explicit claude api": {baseURL: "https://api.anthropic.com"},
		"host case":           {baseURL: "https://API.anthropic.com/"},
		"custom base_url":     {baseURL: "https://proxy.example.com", wantErr: "requires the Claude API"},
		"gateway":             {opts: []options.Opt{options.WithGateway("https://gateway.example.com")}, wantErr: "requires the Claude API"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, fallbacks := range []any{"default", []any{"claude-sonnet-4-6"}} {
				cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-8", BaseURL: tc.baseURL, ProviderOpts: map[string]any{"fallbacks": fallbacks}}
				_, err := NewClient(t.Context(), cfg, env, tc.opts...)
				if tc.wantErr == "" {
					require.NoError(t, err)
					continue
				}
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

// The SDK reads ANTHROPIC_BASE_URL when base_url is unset, so it must pass
// the same check. Not parallel: it sets the process environment.
func TestNewClient_FallbacksCheckEnvBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://litellm.internal")

	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "k"})
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-8", ProviderOpts: map[string]any{"fallbacks": "default"}}
	_, err := NewClient(t.Context(), cfg, env)
	require.ErrorContains(t, err, `not available through base URL "https://litellm.internal"`)

	cfg.BaseURL = "https://api.anthropic.com"
	_, err = NewClient(t.Context(), cfg, env)
	require.NoError(t, err, "an explicit base_url takes precedence over the environment")
}

// The request must be valid for every listed model: a Claude 5.x fallback
// can't take the sampling settings or token budget an older primary sends.
func TestValidateFallbackRequestShape(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		cfg     latest.ModelConfig
		wantErr string
	}{
		"temperature": {
			cfg:     latest.ModelConfig{Model: "claude-opus-4-6", Temperature: new(0.7)},
			wantErr: "sends temperature or top_p",
		},
		"top_k": {
			cfg:     latest.ModelConfig{Model: "claude-opus-4-6", ProviderOpts: map[string]any{"top_k": 5}},
			wantErr: "sends top_k",
		},
		// With thinking on, the request builder doesn't send temperature.
		"temperature with thinking on": {
			cfg: latest.ModelConfig{Model: "claude-opus-4-6", Temperature: new(0.7), ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
		},
		// Sonnet 4.5 has no adaptive thinking: its effort level goes out as a
		// token budget, which a Claude 5.x fallback refuses.
		"effort sent as tokens": {
			cfg:     latest.ModelConfig{Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
			wantErr: "thinks with a token budget",
		},
		"token budget": {
			cfg:     latest.ModelConfig{Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8000}},
			wantErr: "thinks with a token budget",
		},
		"effort":      {cfg: latest.ModelConfig{Model: "claude-opus-4-6", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}},
		"no sampling": {cfg: latest.ModelConfig{Model: "claude-opus-4-6"}},
		// The primary already refuses them, so they're never sent.
		"both refuse sampling":     {cfg: latest.ModelConfig{Model: "claude-fable-5-1", Temperature: new(0.7)}},
		"both refuse token budget": {cfg: latest.ModelConfig{Model: "claude-opus-4-6", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8000}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.Provider = "anthropic"
			if cfg.ProviderOpts == nil {
				cfg.ProviderOpts = map[string]any{}
			}
			cfg.ProviderOpts["fallbacks"] = []any{"claude-sonnet-5-5"}
			err := validateFallbackSpec(&cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
