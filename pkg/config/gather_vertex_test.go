package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func googleModelConfig() *latest.Config {
	return &latest.Config{
		Agents: []latest.AgentConfig{{Name: "a", Model: "m"}},
		Models: map[string]latest.ModelConfig{
			"m": {Provider: "google", Model: "gemini-3.5-flash"},
		},
	}
}

func TestGatherEnvVarsForModels_GoogleVertexFlagIsBoolean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		present bool
		value   string
		want    []string
	}{
		{"unset uses API key", false, "", []string{"GOOGLE_API_KEY"}},
		{"empty uses API key", true, "", []string{"GOOGLE_API_KEY"}},
		{"false uses API key", true, "false", []string{"GOOGLE_API_KEY"}},
		{"FALSE uses API key", true, "FALSE", []string{"GOOGLE_API_KEY"}},
		{"zero uses API key", true, "0", []string{"GOOGLE_API_KEY"}},
		{"true uses Vertex", true, "true", []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_PROJECT"}},
		{"one uses Vertex", true, "1", []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_PROJECT"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			envVars := map[string]string{}
			if tt.present {
				envVars["GOOGLE_GENAI_USE_VERTEXAI"] = tt.value
			}
			got := GatherEnvVarsForModels(t.Context(), googleModelConfig(), environment.NewMapEnvProvider(envVars))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGatherEnvVarsForModels_GoogleCustomProviderWithFalseFlag(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "a", Model: "m"}},
		Models: map[string]latest.ModelConfig{
			"m": {Provider: "google", Model: "gemini-3.5-flash", BaseURL: "https://example.invalid", TokenKey: "CUSTOM_GEMINI_KEY"},
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_GENAI_USE_VERTEXAI": "false",
	})

	assert.Equal(t, []string{"CUSTOM_GEMINI_KEY"}, GatherEnvVarsForModels(t.Context(), cfg, env))
}
