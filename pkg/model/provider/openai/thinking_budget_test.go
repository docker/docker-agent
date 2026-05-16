package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestOpenAIReasoningEffort_Success(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		budget         *latest.ThinkingBudget
		expectedEffort string
	}{
		{"minimal", &latest.ThinkingBudget{Effort: "minimal"}, "minimal"},
		{"low", &latest.ThinkingBudget{Effort: "low"}, "low"},
		{"medium", &latest.ThinkingBudget{Effort: "medium"}, "medium"},
		{"high", &latest.ThinkingBudget{Effort: "high"}, "high"},
		{"xhigh", &latest.ThinkingBudget{Effort: "xhigh"}, "xhigh"},
		{"xhigh uppercase", &latest.ThinkingBudget{Effort: "XHIGH"}, "xhigh"},
		{"uppercase", &latest.ThinkingBudget{Effort: "HIGH"}, "high"},
		{"whitespace", &latest.ThinkingBudget{Effort: "  medium  "}, "medium"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			effort, err := openAIReasoningEffort(tt.budget)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedEffort, effort)
		})
	}
}

func TestOpenAIReasoningEffort_InvalidEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		budget        *latest.ThinkingBudget
		expectedError string
	}{
		{
			name:          "invalid effort level",
			budget:        &latest.ThinkingBudget{Effort: "invalid"},
			expectedError: "got effort: 'invalid', tokens: '0'",
		},
		{
			name:          "numeric string",
			budget:        &latest.ThinkingBudget{Effort: "2048"},
			expectedError: "got effort: '2048', tokens: '0'",
		},
		{
			name:          "tokens set but effort invalid",
			budget:        &latest.ThinkingBudget{Effort: "super-high", Tokens: 4096},
			expectedError: "got effort: 'super-high', tokens: '4096'",
		},
		{
			name:          "tokens only",
			budget:        &latest.ThinkingBudget{Tokens: 2048},
			expectedError: "got effort: '', tokens: '2048'",
		},
		{
			name:          "empty effort",
			budget:        &latest.ThinkingBudget{Effort: ""},
			expectedError: "got effort: '', tokens: '0'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			effort, err := openAIReasoningEffort(tt.budget)
			require.Error(t, err)
			assert.Empty(t, effort)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}
