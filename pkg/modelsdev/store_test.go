package modelsdev

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveModelAlias(t *testing.T) {
	t.Parallel()

	mockData := &Database{
		Providers: map[string]Provider{
			"anthropic": {
				Models: map[string]Model{
					// Pattern 1: alias has same prefix as pinned
					"claude-sonnet-4-5":          {Name: "Claude Sonnet 4.5 (latest)"},
					"claude-sonnet-4-5-20250929": {Name: "Claude Sonnet 4.5"},
					// Pattern 2: alias ends with -0 which gets dropped
					"claude-sonnet-4-0":        {Name: "Claude Sonnet 4 (latest)"},
					"claude-sonnet-4-20250514": {Name: "Claude Sonnet 4"},
					// Pattern 3: -latest suffix style
					"claude-3-5-sonnet-latest":   {Name: "Claude 3.5 Sonnet (latest)"},
					"claude-3-5-sonnet-20241022": {Name: "Claude 3.5 Sonnet"},
					// A pinned model without an alias
					"claude-3-opus-20240229": {Name: "Claude 3 Opus"},
				},
			},
			"openai": {
				Models: map[string]Model{
					"gpt-4o":            {Name: "GPT-4o (latest)"},
					"gpt-4o-2024-11-20": {Name: "GPT-4o"},
				},
			},
		},
	}

	store := NewDatabaseStore(mockData)

	tests := []struct {
		name     string
		provider string
		model    string
		expected string
	}{
		{"resolves alias with same prefix", "anthropic", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929"},
		{"resolves alias with -0 suffix", "anthropic", "claude-sonnet-4-0", "claude-sonnet-4-20250514"},
		{"resolves alias with -latest suffix", "anthropic", "claude-3-5-sonnet-latest", "claude-3-5-sonnet-20241022"},
		{"keeps pinned model unchanged", "anthropic", "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
		{"keeps pinned model without alias unchanged", "anthropic", "claude-3-opus-20240229", "claude-3-opus-20240229"},
		{"resolves openai alias", "openai", "gpt-4o", "gpt-4o-2024-11-20"},
		{"returns original for unknown provider", "unknown", "model", "model"},
		{"returns original for unknown model", "anthropic", "unknown-model", "unknown-model"},
		{"returns original for empty provider", "", "model", "model"},
		{"returns original for empty model", "anthropic", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := store.ResolveModelAlias(t.Context(), tt.provider, tt.model)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDatePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		matches bool
	}{
		{"claude-sonnet-4-5-20250929", true},
		{"gpt-4o-2024-11-20", true},
		{"claude-3-opus-20240229", true},
		{"claude-sonnet-4-5", false},
		{"gpt-4o", false},
		{"some-model-123", false},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			assert.Equal(t, tt.matches, datePattern.MatchString(tt.modelID))
		})
	}
}
