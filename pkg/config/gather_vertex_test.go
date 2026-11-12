package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestGoogleVertexFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		env       map[string]string
		opts      map[string]any
		available bool
		vertex    bool
	}{
		{name: "unset"},
		{name: "empty", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": ""}},
		{name: "false", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}},
		{name: "uppercase false", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "FALSE"}},
		{name: "zero", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "0"}},
		{name: "invalid", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "invalid"}},
		{name: "true", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"}, available: true, vertex: true},
		{name: "uppercase true", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "TRUE"}, available: true, vertex: true},
		{name: "one", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1"}, available: true, vertex: true},
		{name: "false with Google key", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false", "GOOGLE_API_KEY": "false"}, available: true},
		{name: "false with Gemini key", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false", "GEMINI_API_KEY": "false"}, available: true},
		{name: "model garden publisher overrides false", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}, opts: map[string]any{"publisher": "anthropic"}, vertex: true},
		{name: "Google publisher keeps false", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"}, opts: map[string]any{"publisher": "Google"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := environment.NewMapEnvProvider(tt.env)
			cfg := &latest.Config{
				Agents: []latest.AgentConfig{{Name: "root", Model: "gemini"}},
				Models: map[string]latest.ModelConfig{"gemini": {Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: tt.opts}},
			}
			var wantEnv []string
			if tt.vertex {
				wantEnv = []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_PROJECT"}
			} else if tt.env["GEMINI_API_KEY"] == "" {
				wantEnv = []string{"GOOGLE_API_KEY"}
			}
			assert.Equal(t, wantEnv, GatherEnvVarsForModels(t.Context(), cfg, env))

			wantProviders := []string{"dmr"}
			if tt.available {
				wantProviders = []string{"google", "dmr"}
			}
			assert.Equal(t, wantProviders, AvailableProviders(t.Context(), "", env))
		})
	}
}
