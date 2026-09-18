//go:build js

package gemini

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// Without a token source the Vertex backend fails closed instead of probing
// application default credentials from the process.
func TestNewClient_VertexAIWithoutTokenSourceFailsClosed(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/credentials.json")

	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		env  map[string]string
	}{
		{
			name: "project and location",
			cfg:  &latest.ModelConfig{Provider: "google", Model: "gemini-2.0-flash", ProviderOpts: map[string]any{"project": "test-project", "location": "us-central1"}},
		},
		{
			name: "GOOGLE_GENAI_USE_VERTEXAI",
			cfg:  &latest.ModelConfig{Provider: "google", Model: "gemini-2.0-flash"},
			env:  map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1", "GOOGLE_CLOUD_PROJECT": "test-project", "GOOGLE_CLOUD_LOCATION": "us-central1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(t.Context(), tt.cfg, environment.NewMapEnvProvider(tt.env))
			require.ErrorContains(t, err, "requires an explicit token source")
			require.NotContains(t, err.Error(), "credentials.json")
		})
	}
}
