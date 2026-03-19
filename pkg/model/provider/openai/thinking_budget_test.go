package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestIsOpenAIReasoningModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		model    string
		expected bool
	}{
		// O1 series models - should support reasoning
		{"o1-preview", "o1-preview", true},
		{"o1-mini", "o1-mini", true},
		{"O1-PREVIEW uppercase", "O1-PREVIEW", true},
		{"o1 with suffix", "o1-custom-version", true},

		// O3 series models - should support reasoning
		{"o3-mini", "o3-mini", true},
		{"o3-preview", "o3-preview", true},
		{"O3 uppercase", "O3", true},
		{"o3 with suffix", "o3-custom", true},

		// O4 series models - should support reasoning
		{"o4-preview", "o4-preview", true},
		{"o4 basic", "o4", true},
		{"O4 uppercase", "O4", true},

		// GPT-5 series models - should support reasoning
		{"gpt-5", "gpt-5", true},
		{"gpt-5-mini", "gpt-5-mini", true},
		{"gpt-5-turbo", "gpt-5-turbo", true},
		{"GPT-5 uppercase", "GPT-5", true},

		// GPT-5 -chat-latest variants - should NOT support reasoning
		{"gpt-5-chat-latest", "gpt-5-chat-latest", false},
		{"gpt-5-chat-latest with suffix", "gpt-5-chat-latest-2025-10-01", false},
		{"GPT-5-CHAT-LATEST uppercase", "GPT-5-CHAT-LATEST", false},

		// GPT-4 series models - should NOT support reasoning
		{"gpt-4", "gpt-4", false},
		{"gpt-4o", "gpt-4o", false},
		{"gpt-4-turbo", "gpt-4-turbo", false},
		{"gpt-4o-mini", "gpt-4o-mini", false},
		{"GPT-4O uppercase", "GPT-4O", false},

		// GPT-3.5 series models - should NOT support reasoning
		{"gpt-3.5-turbo", "gpt-3.5-turbo", false},
		{"gpt-3.5-turbo-16k", "gpt-3.5-turbo-16k", false},

		// Other models - should NOT support reasoning
		{"text-davinci-003", "text-davinci-003", false},
		{"gpt-3", "gpt-3", false},
		{"claude-3", "claude-3", false},
		{"gemini-pro", "gemini-pro", false},
		{"empty string", "", false},
		{"random model", "random-model-name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := isOpenAIReasoningModel(tt.model)
			assert.Equal(t, tt.expected, result, "Model %s should return %v", tt.model, tt.expected)
		})
	}
}

func TestGetOpenAIReasoningEffort_Success(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		model          string
		thinkingBudget *latest.ThinkingBudget
		expectedEffort string
	}{
		{
			name:  "minimal effort for o1 model",
			model: "o1-preview",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "minimal",
			},
			expectedEffort: "minimal",
		},
		{
			name:  "low effort for gpt-5 model",
			model: "gpt-5-mini",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "low",
			},
			expectedEffort: "low",
		},
		{
			name:  "medium effort for o3 model",
			model: "o3-preview",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "medium",
			},
			expectedEffort: "medium",
		},
		{
			name:  "high effort for o4 model",
			model: "o4",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "high",
			},
			expectedEffort: "high",
		},
		{
			name:  "uppercase effort level",
			model: "o1-mini",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "HIGH",
			},
			expectedEffort: "high",
		},
		{
			name:  "effort with whitespace",
			model: "gpt-5",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "  medium  ",
			},
			expectedEffort: "medium",
		},
		{
			name:           "nil thinking budget",
			model:          "o1-preview",
			thinkingBudget: nil,
			expectedEffort: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &latest.ModelConfig{
				Model:          tt.model,
				ThinkingBudget: tt.thinkingBudget,
			}

			effort, err := getOpenAIReasoningEffort(config)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedEffort, effort)
		})
	}
}

func TestGetOpenAIReasoningEffort_UnsupportedModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
	}{
		{"gpt-4o", "gpt-4o"},
		{"gpt-4-turbo", "gpt-4-turbo"},
		{"gpt-3.5-turbo", "gpt-3.5-turbo"},
		{"claude-3", "claude-3"},
		{"gemini-pro", "gemini-pro"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &latest.ModelConfig{
				Model: tt.model,
				ThinkingBudget: &latest.ThinkingBudget{
					Effort: "high",
				},
			}

			effort, err := getOpenAIReasoningEffort(config)
			require.NoError(t, err)
			assert.Empty(t, effort, "Unsupported model should return empty effort")
		})
	}
}

func TestGetOpenAIReasoningEffort_InvalidEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		thinkingBudget *latest.ThinkingBudget
		expectedError  string
	}{
		{
			name: "invalid effort level",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "invalid",
			},
			expectedError: "OpenAI requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: 'invalid', tokens: '0'",
		},
		{
			name: "numeric string as effort",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "2048",
			},
			expectedError: "OpenAI requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: '2048', tokens: '0'",
		},
		{
			name: "tokens set but effort invalid",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "super-high",
				Tokens: 4096,
			},
			expectedError: "OpenAI requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: 'super-high', tokens: '4096'",
		},
		{
			name: "tokens only (no effort) - should fail",
			thinkingBudget: &latest.ThinkingBudget{
				Tokens: 2048,
			},
			expectedError: "OpenAI requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: '', tokens: '2048'",
		},
		{
			name: "empty effort string - should fail",
			thinkingBudget: &latest.ThinkingBudget{
				Effort: "",
			},
			expectedError: "OpenAI requests only support 'minimal', 'low', 'medium', 'high' as values for thinking_budget effort, got effort: '', tokens: '0'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &latest.ModelConfig{
				Model:          "o1-preview",
				ThinkingBudget: tt.thinkingBudget,
			}

			effort, err := getOpenAIReasoningEffort(config)
			require.Error(t, err)
			assert.Empty(t, effort)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}

func TestGetOpenAIReasoningEffort_NilConfig(t *testing.T) {
	t.Parallel()

	effort, err := getOpenAIReasoningEffort(nil)
	require.NoError(t, err)
	assert.Empty(t, effort)
}

func TestGetOpenAIReasoningEffort_EdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		config         *latest.ModelConfig
		expectedEffort string
		expectedError  bool
	}{
		{
			name: "config with nil thinking budget",
			config: &latest.ModelConfig{
				Model:          "o1-preview",
				ThinkingBudget: nil,
			},
			expectedEffort: "",
			expectedError:  false,
		},
		{
			name: "empty model string with thinking budget",
			config: &latest.ModelConfig{
				Model: "",
				ThinkingBudget: &latest.ThinkingBudget{
					Effort: "high",
				},
			},
			expectedEffort: "",
			expectedError:  false,
		},
		{
			name: "case sensitivity test",
			config: &latest.ModelConfig{
				Model: "O1-PREVIEW",
				ThinkingBudget: &latest.ThinkingBudget{
					Effort: "MINIMAL",
				},
			},
			expectedEffort: "minimal",
			expectedError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			effort, err := getOpenAIReasoningEffort(tt.config)
			if tt.expectedError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.expectedEffort, effort)
		})
	}
}
