package anthropic

import (
	"cmp"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

func TestAnthropicThinkingDisplay(t *testing.T) {
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

// defaultTestModel is an Anthropic model that does NOT require the
// adaptive-thinking workaround, so token-based thinking budgets are
// preserved as-is.
const defaultTestModel = "claude-sonnet-4-5"

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
			name:         "adaptive high effort without display",
			budget:       &latest.ThinkingBudget{Effort: "adaptive"},
			maxTokens:    8192,
			wantEnabled:  true,
			wantAdaptive: true,
			wantEffort:   "high",
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
			name:         "opus-4-6 token budget auto-switches to adaptive",
			model:        "claude-opus-4-6",
			budget:       &latest.ThinkingBudget{Tokens: 4096},
			maxTokens:    8192,
			wantEnabled:  true,
			wantAdaptive: true,
			wantEffort:   "high",
		},
		{
			name:         "opus-4-7 token budget auto-switches to adaptive",
			model:        "claude-opus-4-7",
			budget:       &latest.ThinkingBudget{Tokens: 4096},
			maxTokens:    8192,
			wantEnabled:  true,
			wantAdaptive: true,
			wantEffort:   "high",
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
			name:         "opus-4-6 explicit adaptive budget is preserved",
			model:        "claude-opus-4-6",
			budget:       &latest.ThinkingBudget{Effort: "adaptive/low"},
			maxTokens:    8192,
			wantEnabled:  true,
			wantAdaptive: true,
			wantEffort:   "low",
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
			name:         "opus-4-6 token budget auto-switches to adaptive",
			model:        "claude-opus-4-6",
			budget:       &latest.ThinkingBudget{Tokens: 4096},
			maxTokens:    8192,
			wantAdaptive: true,
			wantEffort:   "high",
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

func TestCoerceAdaptiveThinking(t *testing.T) {
	t.Run("nil budget stays nil", func(t *testing.T) {
		c := clientWithModel("claude-opus-4-7", nil, nil)
		assert.Nil(t, c.coerceAdaptiveThinking())
	})

	t.Run("non-affected model preserves token budget", func(t *testing.T) {
		in := &latest.ThinkingBudget{Tokens: 4096}
		c := clientWithModel(defaultTestModel, in, nil)
		assert.Same(t, in, c.coerceAdaptiveThinking(), "budget pointer must not be replaced")
	})

	t.Run("opus-4-6 token budget is coerced to adaptive", func(t *testing.T) {
		in := &latest.ThinkingBudget{Tokens: 4096}
		c := clientWithModel("claude-opus-4-6", in, nil)
		got := c.coerceAdaptiveThinking()
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
		assert.Same(t, in, c.coerceAdaptiveThinking())
	})

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
			assert.Same(t, in, c.coerceAdaptiveThinking())
		})
	}
}

func TestInterleavedThinkingEnabled(t *testing.T) {
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
