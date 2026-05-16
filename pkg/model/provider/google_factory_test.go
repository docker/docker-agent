package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// withGoogleFactories swaps the gemini and vertex inner factories for the
// duration of a test, restoring the production functions via t.Cleanup.
func withGoogleFactories(t *testing.T, gemini, vertex providerFactory) {
	t.Helper()
	originalGemini, originalVertex := geminiClientFactory, vertexClientFactory
	geminiClientFactory = gemini
	vertexClientFactory = vertex
	t.Cleanup(func() {
		geminiClientFactory = originalGemini
		vertexClientFactory = originalVertex
	})
}

// TestGoogleFactory_RoutesGeminiByDefault verifies that a plain "google" model
// (no Vertex Model Garden hints) is dispatched to the Gemini client.
func TestGoogleFactory_RoutesGeminiByDefault(t *testing.T) {
	withGoogleFactories(t,
		tagFactory("gemini"),
		func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			t.Errorf("vertex factory should not be called for plain Gemini config")
			return nil, errors.New("unreachable")
		},
	)

	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"}

	p, err := googleFactory(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	fp, ok := p.(*fakeProvider)
	require.True(t, ok)
	assert.Equal(t, "gemini", fp.id.Model)
}

// TestGoogleFactory_RoutesGeminiWhenPublisherIsGoogle covers the documented
// edge case in vertexai.IsModelGardenConfig: publisher=google still routes to
// Gemini (it's only a Model Garden config when publisher is non-google).
func TestGoogleFactory_RoutesGeminiWhenPublisherIsGoogle(t *testing.T) {
	withGoogleFactories(t,
		tagFactory("gemini"),
		func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			t.Errorf("vertex factory must not be called when publisher=google")
			return nil, errors.New("unreachable")
		},
	)

	cfg := &latest.ModelConfig{
		Provider:     "google",
		Model:        "gemini-2.5-flash",
		ProviderOpts: map[string]any{"publisher": "google"},
	}

	p, err := googleFactory(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	fp, ok := p.(*fakeProvider)
	require.True(t, ok)
	assert.Equal(t, "gemini", fp.id.Model)
}

// TestGoogleFactory_RoutesVertexForModelGarden verifies that any non-Google
// publisher routes through the Vertex Model Garden factory.
func TestGoogleFactory_RoutesVertexForModelGarden(t *testing.T) {
	withGoogleFactories(t,
		func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			t.Errorf("gemini factory must not be called for Model Garden config")
			return nil, errors.New("unreachable")
		},
		tagFactory("vertex"),
	)

	cfg := &latest.ModelConfig{
		Provider:     "google",
		Model:        "claude-3-5-sonnet@20240620",
		ProviderOpts: map[string]any{"publisher": "anthropic"},
	}

	p, err := googleFactory(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	fp, ok := p.(*fakeProvider)
	require.True(t, ok)
	assert.Equal(t, "vertex", fp.id.Model)
}

// TestGoogleFactory_PropagatesGeminiError verifies that errors from the inner
// gemini factory are surfaced unchanged.
func TestGoogleFactory_PropagatesGeminiError(t *testing.T) {
	sentinel := errors.New("gemini-fail")
	withGoogleFactories(t,
		func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			return nil, sentinel
		},
		tagFactory("vertex"),
	)

	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"}

	_, err := googleFactory(t.Context(), cfg, environment.NewNoEnvProvider())
	require.ErrorIs(t, err, sentinel)
}

// TestGoogleFactory_PropagatesVertexError verifies that errors from the inner
// vertex factory are surfaced unchanged.
func TestGoogleFactory_PropagatesVertexError(t *testing.T) {
	sentinel := errors.New("vertex-fail")
	withGoogleFactories(t,
		tagFactory("gemini"),
		func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			return nil, sentinel
		},
	)

	cfg := &latest.ModelConfig{
		Provider:     "google",
		Model:        "claude-3-5-sonnet@20240620",
		ProviderOpts: map[string]any{"publisher": "anthropic"},
	}

	_, err := googleFactory(t.Context(), cfg, environment.NewNoEnvProvider())
	require.ErrorIs(t, err, sentinel)
}
