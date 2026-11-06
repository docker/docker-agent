package anthropic

import (
	"cmp"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestAnthropicThinkingDisplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   map[string]any
		want   string
		wantOk bool
	}{
		{
			name:   "nil opts",
			opts:   nil,
			want:   "",
			wantOk: false,
		},
		{
			name:   "empty opts",
			opts:   map[string]any{},
			want:   "",
			wantOk: false,
		},
		{
			name:   "key missing",
			opts:   map[string]any{"other": "foo"},
			want:   "",
			wantOk: false,
		},
		{
			name:   "summarized",
			opts:   map[string]any{"thinking_display": "summarized"},
			want:   "summarized",
			wantOk: true,
		},
		{
			name:   "omitted",
			opts:   map[string]any{"thinking_display": "omitted"},
			want:   "omitted",
			wantOk: true,
		},
		{
			name:   "display",
			opts:   map[string]any{"thinking_display": "display"},
			want:   "display",
			wantOk: true,
		},
		{
			name:   "case insensitive",
			opts:   map[string]any{"thinking_display": "SUMMARIZED"},
			want:   "summarized",
			wantOk: true,
		},
		{
			name:   "whitespace trimmed",
			opts:   map[string]any{"thinking_display": "  omitted  "},
			want:   "omitted",
			wantOk: true,
		},
		{
			name:   "invalid string",
			opts:   map[string]any{"thinking_display": "not-a-valid-value"},
			want:   "",
			wantOk: false,
		},
		{
			name:   "non-string value",
			opts:   map[string]any{"thinking_display": 42},
			want:   "",
			wantOk: false,
		},
		{
			name:   "bool value",
			opts:   map[string]any{"thinking_display": true},
			want:   "",
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := anthropicThinkingDisplay(tt.opts)
			assert.Equal(t, tt.wantOk, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateThinkingDisplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		model   string
		opts    map[string]any
		wantErr string
	}{
		{
			name:  "no thinking_display",
			model: "claude-sonnet-5",
		},
		{
			name:  "summarized on adaptive model",
			model: "claude-sonnet-5",
			opts:  map[string]any{"thinking_display": "summarized"},
		},
		{
			name:  "omitted on adaptive model",
			model: "claude-opus-4-7",
			opts:  map[string]any{"thinking_display": "omitted"},
		},
		{
			name:  "display on token-thinking model",
			model: "claude-sonnet-4-5",
			opts:  map[string]any{"thinking_display": "display"},
		},
		{
			name:    "display on claude-sonnet-5",
			model:   "claude-sonnet-5",
			opts:    map[string]any{"thinking_display": "display"},
			wantErr: `does not support thinking_display: "display"`,
		},
		{
			name:    "display on claude-opus-4-7",
			model:   "claude-opus-4-7",
			opts:    map[string]any{"thinking_display": "display"},
			wantErr: `does not support thinking_display: "display"`,
		},
		{
			// Invalid values keep their warn-and-ignore behavior.
			name:  "invalid value ignored",
			model: "claude-sonnet-5",
			opts:  map[string]any{"thinking_display": "bogus"},
		},
		{
			name:  "display with supported fallback",
			model: "claude-sonnet-4-5",
			opts: map[string]any{
				"thinking_display": "display",
				"fallbacks":        []any{"claude-haiku-4-5"},
			},
		},
		{
			// Fallbacks receive the exact same request shape, so an
			// unsupported fallback must be rejected too.
			name:  "display with unsupported fallback",
			model: "claude-sonnet-4-5",
			opts: map[string]any{
				"thinking_display": "display",
				"fallbacks":        []any{"claude-haiku-4-5", "claude-sonnet-4-6"},
			},
			wantErr: `model "claude-sonnet-4-6" does not support thinking_display: "display"`,
		},
		{
			// The server picks default targets, so only the primary is checked.
			name:  "display with default fallbacks",
			model: "claude-sonnet-4-5",
			opts:  map[string]any{"thinking_display": "display", "fallbacks": "default"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateThinkingDisplay(&latest.ModelConfig{Model: tt.model, ProviderOpts: tt.opts})
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNewClient_RejectsFullThinkingDisplayOnUnsupportedModel(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{
		Provider:     "anthropic",
		Model:        "claude-sonnet-5",
		ProviderOpts: map[string]any{"thinking_display": "display"},
	}
	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support thinking_display")
}

// defaultTestModel is an Anthropic model that supports adaptive thinking but is
// NOT in the token-rejecting set (Opus 4.6+), so effort/adaptive budgets use the
// adaptive-thinking API while token-based budgets are preserved as-is.
const defaultTestModel = "claude-sonnet-4-6"

// nonAdaptiveTestModel is an Anthropic model that does NOT support adaptive
// thinking (issue #3362), so effort/adaptive budgets must fall back to
// token-based extended thinking.
const nonAdaptiveTestModel = "claude-haiku-4-5"

// clientWith builds a minimal Client with the given ThinkingBudget and
// provider_opts on defaultTestModel.
func clientWith(budget *latest.ThinkingBudget, opts map[string]any) *Client {
	return clientWithModel(defaultTestModel, budget, opts)
}

// clientWithModel is like clientWith but lets the test pick the model name,
// which matters for behaviors like the Opus 4.6/4.7 adaptive-thinking switch.
func clientWithModel(model string, budget *latest.ThinkingBudget, opts map[string]any) *Client {
	return &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:       "anthropic",
				Model:          model,
				ThinkingBudget: budget,
				ProviderOpts:   opts,
			},
		},
	}
}

func TestApplyThinkingConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		model           string // optional; defaults to a non-Opus-4-6/4-7 model.
		budget          *latest.ThinkingBudget
		opts            map[string]any
		maxTokens       int64
		wantEnabled     bool
		wantAdaptive    bool
		wantTokens      int64
		wantEffort      string
		wantDisplayJSON string // "" means the display field must not be present in JSON
	}{
		{
			name:        "nil budget disables thinking",
			budget:      nil,
			maxTokens:   8192,
			wantEnabled: false,
		},
		{
			name:        "token budget below minimum is ignored",
			budget:      &latest.ThinkingBudget{Tokens: 500},
			maxTokens:   8192,
			wantEnabled: false,
		},
		{
			name:        "token budget above max_tokens is ignored",
			budget:      &latest.ThinkingBudget{Tokens: 9000},
			maxTokens:   8192,
			wantEnabled: false,
		},
		{
			name:            "adaptive high effort without display defaults to summarized",
			budget:          &latest.ThinkingBudget{Effort: "adaptive"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "adaptive with display=summarized",
			budget:          &latest.ThinkingBudget{Effort: "adaptive"},
			opts:            map[string]any{"thinking_display": "summarized"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "adaptive with display=omitted",
			budget:          &latest.ThinkingBudget{Effort: "adaptive/low"},
			opts:            map[string]any{"thinking_display": "omitted"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "low",
			wantDisplayJSON: "omitted",
		},
		{
			name:        "token budget without display",
			budget:      &latest.ThinkingBudget{Tokens: 2048},
			maxTokens:   8192,
			wantEnabled: true,
			wantTokens:  2048,
		},
		{
			name:            "token budget with display=display",
			budget:          &latest.ThinkingBudget{Tokens: 2048},
			opts:            map[string]any{"thinking_display": "display"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantTokens:      2048,
			wantDisplayJSON: "display",
		},
		{
			name:        "invalid display value is ignored",
			budget:      &latest.ThinkingBudget{Tokens: 2048},
			opts:        map[string]any{"thinking_display": "bogus"},
			maxTokens:   8192,
			wantEnabled: true,
			wantTokens:  2048,
		},
		{
			name:            "opus-4-6 token budget auto-switches to adaptive",
			model:           "claude-opus-4-6",
			budget:          &latest.ThinkingBudget{Tokens: 4096},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "opus-4-7 token budget auto-switches to adaptive",
			model:           "claude-opus-4-7",
			budget:          &latest.ThinkingBudget{Tokens: 4096},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "opus-4-6 dated variant token budget auto-switches to adaptive",
			model:           "claude-opus-4-6-20251101",
			budget:          &latest.ThinkingBudget{Tokens: 8000},
			opts:            map[string]any{"thinking_display": "summarized"},
			maxTokens:       16384,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "opus-4-6 explicit adaptive budget is preserved",
			model:           "claude-opus-4-6",
			budget:          &latest.ThinkingBudget{Effort: "adaptive/low"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "low",
			wantDisplayJSON: "summarized",
		},
		// Issue #3362: an effort level (as set via the TUI Shift+Tab cycle) uses
		// adaptive thinking only on models that support it, and falls back to a
		// token budget everywhere else.
		{
			name:            "plain effort level on adaptive model uses adaptive",
			budget:          &latest.ThinkingBudget{Effort: "high"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		// Fable 5 omits thinking content server-side by default; the effort
		// levels set via /effort or Shift+Tab must request summarized thinking
		// so reasoning stays visible in the TUI.
		{
			name:            "fable-5 effort level defaults display to summarized",
			model:           "claude-fable-5",
			budget:          &latest.ThinkingBudget{Effort: "max"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "max",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "fable-5 explicit omitted display is preserved",
			model:           "claude-fable-5",
			budget:          &latest.ThinkingBudget{Effort: "high"},
			opts:            map[string]any{"thinking_display": "omitted"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "omitted",
		},
		{
			name:        "effort level on non-adaptive model falls back to token budget",
			model:       "claude-haiku-4-5",
			budget:      &latest.ThinkingBudget{Effort: "medium"},
			maxTokens:   16384,
			wantEnabled: true,
			wantTokens:  8192,
		},
		{
			name:        "effort high on non-adaptive model maps to 16384 tokens",
			model:       "claude-haiku-4-5",
			budget:      &latest.ThinkingBudget{Effort: "high"},
			maxTokens:   32768,
			wantEnabled: true,
			wantTokens:  16384,
		},
		{
			name:        "adaptive budget on non-adaptive model falls back to token budget",
			model:       "claude-haiku-4-5",
			budget:      &latest.ThinkingBudget{Effort: "adaptive"},
			maxTokens:   32768,
			wantEnabled: true,
			wantTokens:  16384,
		},
		{
			name:            "non-adaptive token fallback keeps display",
			model:           "claude-haiku-4-5",
			budget:          &latest.ThinkingBudget{Effort: "low"},
			opts:            map[string]any{"thinking_display": "omitted"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantTokens:      2048,
			wantDisplayJSON: "omitted",
		},
		{
			name:        "non-adaptive token fallback dropped when exceeding max_tokens",
			model:       "claude-haiku-4-5",
			budget:      &latest.ThinkingBudget{Effort: "high"}, // 16384 > maxTokens
			maxTokens:   8192,
			wantEnabled: false,
		},
		{
			name:        "sonnet-4-5 effort falls back to token budget",
			model:       "claude-sonnet-4-5",
			budget:      &latest.ThinkingBudget{Effort: "high"},
			maxTokens:   32768,
			wantEnabled: true,
			wantTokens:  16384,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := clientWithModel(cmp.Or(tt.model, defaultTestModel), tt.budget, tt.opts)
			params := anthropic.MessageNewParams{}

			gotEnabled := c.applyThinkingConfig(&params, tt.maxTokens)
			assert.Equal(t, tt.wantEnabled, gotEnabled)

			if !tt.wantEnabled {
				assert.Nil(t, params.Thinking.OfAdaptive)
				assert.Nil(t, params.Thinking.OfEnabled)
				return
			}

			if tt.wantAdaptive {
				require.NotNil(t, params.Thinking.OfAdaptive)
				assert.Equal(t, tt.wantEffort, string(params.OutputConfig.Effort))
				assert.Equal(t, tt.wantDisplayJSON, string(params.Thinking.OfAdaptive.Display))
			} else {
				require.NotNil(t, params.Thinking.OfEnabled)
				assert.Equal(t, tt.wantTokens, params.Thinking.OfEnabled.BudgetTokens)
				assert.Equal(t, tt.wantDisplayJSON, string(params.Thinking.OfEnabled.Display))
			}

			// Sanity-check: the marshaled JSON omits display entirely when unset,
			// thanks to the SDK's `json:"display,omitzero"` tag.
			b, err := json.Marshal(params.Thinking)
			require.NoError(t, err)
			if tt.wantDisplayJSON == "" {
				assert.NotContains(t, string(b), `"display"`)
			} else {
				assert.Contains(t, string(b), `"display":"`+tt.wantDisplayJSON+`"`)
			}
		})
	}
}

func TestApplyBetaThinkingConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		model           string // optional; defaults to a non-Opus-4-6/4-7 model.
		budget          *latest.ThinkingBudget
		opts            map[string]any
		maxTokens       int64
		wantAdaptive    bool
		wantEnabled     bool
		wantTokens      int64
		wantEffort      string
		wantDisplayJSON string
	}{
		{
			name:      "nil budget leaves params untouched",
			budget:    nil,
			maxTokens: 8192,
		},
		{
			name:            "adaptive with display",
			budget:          &latest.ThinkingBudget{Effort: "adaptive/medium"},
			opts:            map[string]any{"thinking_display": "display"},
			maxTokens:       8192,
			wantAdaptive:    true,
			wantEffort:      "medium",
			wantDisplayJSON: "display",
		},
		{
			name:            "token budget with display=omitted",
			budget:          &latest.ThinkingBudget{Tokens: 4096},
			opts:            map[string]any{"thinking_display": "omitted"},
			maxTokens:       8192,
			wantEnabled:     true,
			wantTokens:      4096,
			wantDisplayJSON: "omitted",
		},
		{
			name:      "invalid token budget leaves params untouched",
			budget:    &latest.ThinkingBudget{Tokens: 100},
			maxTokens: 8192,
		},
		{
			name:            "opus-4-6 token budget auto-switches to adaptive",
			model:           "claude-opus-4-6",
			budget:          &latest.ThinkingBudget{Tokens: 4096},
			maxTokens:       8192,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "opus-4-7 token budget auto-switches to adaptive with display",
			model:           "claude-opus-4-7",
			budget:          &latest.ThinkingBudget{Tokens: 4096},
			opts:            map[string]any{"thinking_display": "omitted"},
			maxTokens:       8192,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "omitted",
		},
		// Issue #3362: effort/adaptive budgets fall back to token thinking on
		// models without adaptive support.
		{
			name:            "plain effort on adaptive model uses adaptive",
			budget:          &latest.ThinkingBudget{Effort: "high"},
			maxTokens:       8192,
			wantAdaptive:    true,
			wantEffort:      "high",
			wantDisplayJSON: "summarized",
		},
		{
			name:            "fable-5 effort level defaults display to summarized",
			model:           "claude-fable-5",
			budget:          &latest.ThinkingBudget{Effort: "xhigh"},
			maxTokens:       8192,
			wantAdaptive:    true,
			wantEffort:      "xhigh",
			wantDisplayJSON: "summarized",
		},
		{
			name:        "effort on non-adaptive model falls back to token budget",
			model:       "claude-haiku-4-5",
			budget:      &latest.ThinkingBudget{Effort: "medium"},
			maxTokens:   16384,
			wantEnabled: true,
			wantTokens:  8192,
		},
		{
			name:            "adaptive on non-adaptive model falls back to token budget with display",
			model:           "claude-haiku-4-5",
			budget:          &latest.ThinkingBudget{Effort: "adaptive"},
			opts:            map[string]any{"thinking_display": "omitted"},
			maxTokens:       32768,
			wantEnabled:     true,
			wantTokens:      16384,
			wantDisplayJSON: "omitted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := clientWithModel(cmp.Or(tt.model, defaultTestModel), tt.budget, tt.opts)
			params := anthropic.BetaMessageNewParams{}

			c.applyBetaThinkingConfig(&params, tt.maxTokens)

			switch {
			case tt.wantAdaptive:
				require.NotNil(t, params.Thinking.OfAdaptive)
				assert.Equal(t, tt.wantEffort, string(params.OutputConfig.Effort))
				assert.Equal(t, tt.wantDisplayJSON, string(params.Thinking.OfAdaptive.Display))
			case tt.wantEnabled:
				require.NotNil(t, params.Thinking.OfEnabled)
				assert.Equal(t, tt.wantTokens, params.Thinking.OfEnabled.BudgetTokens)
				assert.Equal(t, tt.wantDisplayJSON, string(params.Thinking.OfEnabled.Display))
			default:
				assert.Nil(t, params.Thinking.OfAdaptive)
				assert.Nil(t, params.Thinking.OfEnabled)
			}
		})
	}
}

func TestAdjustMaxTokensForThinking(t *testing.T) {
	t.Parallel()
	t.Run("no budget returns input unchanged", func(t *testing.T) {
		c := clientWith(nil, nil)
		got, err := c.adjustMaxTokensForThinking(8192)
		require.NoError(t, err)
		assert.Equal(t, int64(8192), got)
	})

	t.Run("adaptive budget returns input unchanged", func(t *testing.T) {
		c := clientWith(&latest.ThinkingBudget{Effort: "adaptive"}, nil)
		got, err := c.adjustMaxTokensForThinking(8192)
		require.NoError(t, err)
		assert.Equal(t, int64(8192), got)
	})

	t.Run("token budget fits in max_tokens", func(t *testing.T) {
		c := clientWith(&latest.ThinkingBudget{Tokens: 2048}, nil)
		got, err := c.adjustMaxTokensForThinking(8192)
		require.NoError(t, err)
		assert.Equal(t, int64(8192), got)
	})

	t.Run("auto-adjust when user didn't set max_tokens", func(t *testing.T) {
		c := clientWith(&latest.ThinkingBudget{Tokens: 16384}, nil)
		got, err := c.adjustMaxTokensForThinking(8192)
		require.NoError(t, err)
		assert.Equal(t, int64(16384+8192), got)
	})

	t.Run("error when user explicitly set max_tokens too low", func(t *testing.T) {
		c := clientWith(&latest.ThinkingBudget{Tokens: 16384}, nil)
		userMax := int64(8192)
		c.ModelConfig.MaxTokens = &userMax
		_, err := c.adjustMaxTokensForThinking(8192)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_tokens")
	})

	t.Run("opus-4-6 with token budget skips adjustment (will be coerced to adaptive)", func(t *testing.T) {
		c := clientWithModel("claude-opus-4-6", &latest.ThinkingBudget{Tokens: 16384}, nil)
		got, err := c.adjustMaxTokensForThinking(8192)
		require.NoError(t, err)
		assert.Equal(t, int64(8192), got)
	})

	t.Run("opus-4-7 with token budget skips adjustment (will be coerced to adaptive)", func(t *testing.T) {
		c := clientWithModel("claude-opus-4-7-20251101", &latest.ThinkingBudget{Tokens: 16384}, nil)
		userMax := int64(8192)
		c.ModelConfig.MaxTokens = &userMax
		got, err := c.adjustMaxTokensForThinking(8192)
		require.NoError(t, err)
		assert.Equal(t, int64(8192), got)
	})
}

func TestResolveThinkingBudget(t *testing.T) {
	t.Parallel()
	t.Run("nil budget stays nil", func(t *testing.T) {
		c := clientWithModel("claude-opus-4-7", nil, nil)
		assert.Nil(t, c.resolveThinkingBudget())
	})

	t.Run("non-affected model preserves token budget", func(t *testing.T) {
		in := &latest.ThinkingBudget{Tokens: 4096}
		c := clientWithModel(defaultTestModel, in, nil)
		assert.Same(t, in, c.resolveThinkingBudget(), "budget pointer must not be replaced")
	})

	t.Run("adaptive-capable model preserves effort budget", func(t *testing.T) {
		in := &latest.ThinkingBudget{Effort: "high"}
		c := clientWithModel(defaultTestModel, in, nil)
		assert.Same(t, in, c.resolveThinkingBudget())
	})

	t.Run("opus-4-6 token budget is coerced to adaptive", func(t *testing.T) {
		in := &latest.ThinkingBudget{Tokens: 4096}
		c := clientWithModel("claude-opus-4-6", in, nil)
		got := c.resolveThinkingBudget()
		require.NotNil(t, got)
		assert.Equal(t, "adaptive", got.Effort)
		assert.Equal(t, 0, got.Tokens)
		// Original must not be mutated.
		assert.Equal(t, 4096, in.Tokens)
		assert.Empty(t, in.Effort)
	})

	t.Run("opus-4-7 adaptive budget is preserved as-is", func(t *testing.T) {
		in := &latest.ThinkingBudget{Effort: "adaptive/low"}
		c := clientWithModel("claude-opus-4-7", in, nil)
		assert.Same(t, in, c.resolveThinkingBudget())
	})

	// Issue #3362: effort/adaptive budgets on models without adaptive-thinking
	// support fall back to a token budget instead of a 400.
	effortFallbackCases := map[string]struct {
		budget     *latest.ThinkingBudget
		wantTokens int
	}{
		"effort high -> 16384":     {&latest.ThinkingBudget{Effort: "high"}, 16384},
		"effort medium -> 8192":    {&latest.ThinkingBudget{Effort: "medium"}, 8192},
		"effort low -> 2048":       {&latest.ThinkingBudget{Effort: "low"}, 2048},
		"adaptive -> 16384 (high)": {&latest.ThinkingBudget{Effort: "adaptive"}, 16384},
		"adaptive/low -> 2048":     {&latest.ThinkingBudget{Effort: "adaptive/low"}, 2048},
	}
	for name, tc := range effortFallbackCases {
		t.Run("haiku-4-5 "+name, func(t *testing.T) {
			c := clientWithModel(nonAdaptiveTestModel, tc.budget, nil)
			got := c.resolveThinkingBudget()
			require.NotNil(t, got)
			assert.Equal(t, tc.wantTokens, got.Tokens)
			assert.Empty(t, got.Effort)
			// Original must not be mutated.
			assert.Empty(t, tc.budget.Tokens)
		})
	}

	// Disabled or non-positive token budgets must NOT be silently coerced to
	// adaptive thinking on Opus 4.6/4.7 — the user has either explicitly
	// disabled thinking or supplied an invalid value.
	disabledCases := map[string]*latest.ThinkingBudget{
		"thinking_budget: 0":            {Tokens: 0},
		"thinking_budget: none":         {Effort: "none"},
		"effort=none with stray tokens": {Effort: "none", Tokens: 99},
		"negative tokens":               {Tokens: -5},
	}
	for name, in := range disabledCases {
		t.Run("opus-4-7 "+name+" passes through", func(t *testing.T) {
			c := clientWithModel("claude-opus-4-7", in, nil)
			assert.Same(t, in, c.resolveThinkingBudget())
		})
	}
}

func TestFloorMaxTokensForNoThinking(t *testing.T) {
	t.Parallel()
	buildOpts := func(opts ...options.Opt) options.ModelOptions {
		var mo options.ModelOptions
		for _, opt := range opts {
			opt(&mo)
		}
		return mo
	}

	tests := []struct {
		name      string
		opts      options.ModelOptions
		maxTokens int64
		want      int64
	}{
		{
			name:      "no-thinking tiny cap is raised to floor",
			opts:      buildOpts(options.WithNoThinking(), options.WithMaxTokens(20)),
			maxTokens: 20,
			want:      noThinkingMinOutputTokens,
		},
		{
			name:      "no-thinking cap already above floor is unchanged",
			opts:      buildOpts(options.WithNoThinking()),
			maxTokens: 8192,
			want:      8192,
		},
		{
			name:      "no-thinking cap equal to floor is unchanged",
			opts:      buildOpts(options.WithNoThinking()),
			maxTokens: noThinkingMinOutputTokens,
			want:      noThinkingMinOutputTokens,
		},
		{
			name:      "thinking enabled leaves tiny cap untouched",
			opts:      buildOpts(options.WithMaxTokens(20)),
			maxTokens: 20,
			want:      20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{Config: base.Config{ModelOptions: tt.opts}}
			assert.Equal(t, tt.want, c.floorMaxTokensForNoThinking(tt.maxTokens))
		})
	}
}

func TestInterleavedThinkingEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts map[string]any
		want bool
	}{
		{"nil opts", nil, false},
		{"missing key", map[string]any{"other": true}, false},
		{"bool true", map[string]any{"interleaved_thinking": true}, true},
		{"bool false", map[string]any{"interleaved_thinking": false}, false},
		{"string true", map[string]any{"interleaved_thinking": "true"}, true},
		{"string false", map[string]any{"interleaved_thinking": "false"}, false},
		{"string no", map[string]any{"interleaved_thinking": "no"}, false},
		{"int 0", map[string]any{"interleaved_thinking": 0}, false},
		{"int 1", map[string]any{"interleaved_thinking": 1}, true},
		{"unsupported type", map[string]any{"interleaved_thinking": []string{}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := clientWith(nil, tt.opts)
			assert.Equal(t, tt.want, c.interleavedThinkingEnabled())
		})
	}
}
