package openai

import (
	"encoding/json"
	"testing"

	oai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyProviderOptsExtraFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		opts     map[string]any
		wantKeys []string // keys expected in JSON output
	}{
		{
			name: "nil opts",
			opts: nil,
		},
		{
			name: "empty opts",
			opts: map[string]any{},
		},
		{
			name:     "top_k forwarded",
			opts:     map[string]any{"top_k": 40},
			wantKeys: []string{"top_k"},
		},
		{
			name:     "repetition_penalty forwarded",
			opts:     map[string]any{"repetition_penalty": 1.15},
			wantKeys: []string{"repetition_penalty"},
		},
		{
			name:     "multiple sampling opts",
			opts:     map[string]any{"top_k": 50, "repetition_penalty": 1.1, "min_p": 0.05},
			wantKeys: []string{"top_k", "repetition_penalty", "min_p"},
		},
		{
			name: "non-sampling opts ignored",
			opts: map[string]any{"api_type": "openai_chatcompletions", "transport": "websocket"},
		},
		{
			name:     "seed set natively",
			opts:     map[string]any{"seed": 42},
			wantKeys: []string{"seed"},
		},
		{
			name:     "extra_body forwarded verbatim",
			opts:     map[string]any{"extra_body": map[string]any{"reasoning_effort": "none", "chat_template_kwargs": map[string]any{"enable_thinking": false}}},
			wantKeys: []string{"reasoning_effort", "chat_template_kwargs"},
		},
		{
			name:     "extra_body alongside sampling opts",
			opts:     map[string]any{"top_k": 40, "extra_body": map[string]any{"reasoning_effort": "none"}},
			wantKeys: []string{"top_k", "reasoning_effort"},
		},
		{
			name: "extra_body of the wrong type ignored",
			opts: map[string]any{"extra_body": "reasoning_effort=none"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := oai.ChatCompletionNewParams{
				Model: "test-model",
			}
			applyProviderOptsExtraFields(&params, tt.opts, nil)

			// Marshal to JSON and check for expected keys
			data, err := json.Marshal(params)
			require.NoError(t, err)

			var m map[string]any
			require.NoError(t, json.Unmarshal(data, &m))

			for _, key := range tt.wantKeys {
				assert.Contains(t, m, key, "expected key %q in JSON output", key)
			}

			// Non-sampling keys should never appear
			assert.NotContains(t, m, "api_type")
			assert.NotContains(t, m, "transport")
		})
	}
}
