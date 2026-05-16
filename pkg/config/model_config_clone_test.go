package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestModelConfig_Clone_DeepCopiesPointerFields(t *testing.T) {
	temperature := 0.7
	maxTokens := int64(2048)
	topP := 0.9
	freqPenalty := 0.1
	presencePenalty := 0.2
	parallel := true
	trackUsage := false

	original := &latest.ModelConfig{
		Provider:          "openai",
		Model:             "gpt-4o",
		DisplayModel:      "gpt-4o-display",
		Temperature:       &temperature,
		MaxTokens:         &maxTokens,
		TopP:              &topP,
		FrequencyPenalty:  &freqPenalty,
		PresencePenalty:   &presencePenalty,
		BaseURL:           "https://api.openai.com/v1",
		ParallelToolCalls: &parallel,
		TokenKey:          "OPENAI_API_KEY",
		ProviderOpts:      map[string]any{"organization": "org-123"},
		TrackUsage:        &trackUsage,
		ThinkingBudget:    &latest.ThinkingBudget{Tokens: 5000},
		Routing: []latest.RoutingRule{
			{Model: "openai/gpt-4o-mini", Examples: []string{"simple query"}},
		},
	}

	clone := original.Clone()
	require.NotNil(t, clone)

	// Verify all fields are equal
	assert.Equal(t, original.Provider, clone.Provider)
	assert.Equal(t, original.Model, clone.Model)
	assert.Equal(t, original.DisplayModel, clone.DisplayModel)
	assert.InEpsilon(t, *original.Temperature, *clone.Temperature, 0.001)
	assert.Equal(t, *original.MaxTokens, *clone.MaxTokens)

	// Verify pointer fields are independent (deep copy)
	*clone.Temperature = 0.5
	assert.InEpsilon(t, 0.7, *original.Temperature, 0.001, "modifying clone should not affect original")

	clone.ProviderOpts["organization"] = "org-456"
	assert.Equal(t, "org-123", original.ProviderOpts["organization"], "modifying clone map should not affect original")

	clone.Routing[0].Model = "different/model"
	assert.Equal(t, "openai/gpt-4o-mini", original.Routing[0].Model, "modifying clone routing should not affect original")
}

func TestModelConfig_Clone_Nil(t *testing.T) {
	var nilConfig *latest.ModelConfig
	clone := nilConfig.Clone()
	assert.Nil(t, clone)
}

func TestModelConfig_Clone_MinimalFields(t *testing.T) {
	original := &latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-sonnet-4-5",
	}

	clone := original.Clone()
	require.NotNil(t, clone)

	assert.Equal(t, original.Provider, clone.Provider)
	assert.Equal(t, original.Model, clone.Model)
	assert.Nil(t, clone.Temperature)
	assert.Nil(t, clone.MaxTokens)
	assert.Empty(t, clone.ProviderOpts)
	assert.Empty(t, clone.Routing)
}
