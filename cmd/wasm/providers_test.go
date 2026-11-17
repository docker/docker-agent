//go:build js && wasm

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider"
)

func TestDemoProviderRegistry(t *testing.T) {
	for _, name := range []string{"openai", "openai_chatcompletions", "openai_responses", "anthropic", "google"} {
		assert.True(t, demoProviders.Has(name), name)
		assert.False(t, provider.EmptyRegistry().Has(name), "demo registration must not affect the empty core registry")
	}
}

func TestBrowserHostBuildsDemoProvidersWithSessionEnv(t *testing.T) {
	const yaml = `
models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-6
  fallback:
    provider: openai
    model: gpt-4o-mini
agents:
  root:
    model: primary
    fallback:
      models: [fallback]
`
	// Keys come from the session env only; nothing is exported to the process.
	s, err := browserHost.openSession(t.Context(), sessionOptions{YAML: yaml, Env: map[string]string{"ANTHROPIC_API_KEY": "test", "OPENAI_API_KEY": "test"}})
	require.NoError(t, err)
	require.NoError(t, s.close())

	_, err = browserHost.openSession(t.Context(), sessionOptions{YAML: yaml})
	require.Error(t, err, "missing keys are reported when the session is created")
}
