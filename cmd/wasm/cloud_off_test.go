//go:build js && wasm && !docker_agent_wasm_cloud

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// Without the docker_agent_wasm_cloud tag the demo links no cloud SDK.
func TestDemoProviderRegistryHasNoCloudProviders(t *testing.T) {
	assert.Nil(t, cloudFactories())
	for _, name := range []string{"amazon-bedrock", "dmr"} {
		assert.False(t, demoProviders.Has(name), name)
	}
}

// Vertex-shaped google models fail with a pointer to the cloud build instead
// of reaching the Vertex backend, whose ADC lookup would read
// GOOGLE_APPLICATION_CREDENTIALS from the process env.
func TestGoogleFactoryRejectsVertexWithoutCloudTag(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/credentials.json")

	tests := []struct {
		name string
		opts map[string]any
		env  map[string]string
	}{
		{name: "project and location", opts: map[string]any{"project": "my-project", "location": "us-central1"}},
		{name: "project only", opts: map[string]any{"project": "my-project"}},
		{name: "location only", opts: map[string]any{"location": "us-central1"}},
		{name: "model garden publisher", opts: map[string]any{"publisher": "anthropic"}},
		{
			name: "GOOGLE_GENAI_USE_VERTEXAI",
			env:  map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1", "GOOGLE_CLOUD_PROJECT": "my-project", "GOOGLE_CLOUD_LOCATION": "us-central1"},
		},
		{
			name: "token alone does not help",
			opts: map[string]any{"project": "my-project", "location": "us-central1"},
			env:  map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "tok", "GOOGLE_API_KEY": "key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: tt.opts}
			_, err := demoProviders.New(t.Context(), cfg, environment.NewMapEnvProvider(tt.env))
			require.ErrorContains(t, err, "-tags docker_agent_wasm_cloud")
			require.ErrorContains(t, err, "GOOGLE_OAUTH_ACCESS_TOKEN")
			assert.NotContains(t, err.Error(), "credentials.json")
		})
	}

	yaml := "models:\n  primary:\n    provider: google\n    model: gemini-2.5-flash\n    provider_opts:\n      project: my-project\n      location: us-central1\nagents:\n  root:\n    model: primary\n"
	_, err := browserHost.openSession(t.Context(), sessionOptions{YAML: yaml})
	require.ErrorContains(t, err, "-tags docker_agent_wasm_cloud", "rejected when the session is created")
}

func TestGoogleFactoryKeepsGeminiAPIWithoutCloudTag(t *testing.T) {
	const yaml = "models:\n  primary:\n    provider: google\n    model: gemini-2.5-flash\nagents:\n  root:\n    model: primary\n"
	s, err := browserHost.openSession(t.Context(), sessionOptions{YAML: yaml, Env: map[string]string{"GOOGLE_API_KEY": "key"}})
	require.NoError(t, err)
	require.NoError(t, s.close())

	s, err = browserHost.openSession(t.Context(), sessionOptions{YAML: yaml, Env: map[string]string{"GOOGLE_API_KEY": "key", "GOOGLE_CLOUD_PROJECT": "p", "GOOGLE_CLOUD_LOCATION": "l"}})
	require.NoError(t, err, "GOOGLE_CLOUD_PROJECT alone does not select Vertex AI")
	require.NoError(t, s.close())
}
