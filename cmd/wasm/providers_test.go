//go:build js && wasm

package main

import (
	"syscall/js"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
)

func TestDemoProviderRegistry(t *testing.T) {
	for _, name := range []string{"openai", "openai_chatcompletions", "openai_responses", "anthropic", "google"} {
		assert.True(t, demoProviders.Has(name), name)
		assert.False(t, provider.DefaultRegistry().Has(name), "demo registration must not affect core defaults")
	}
	assert.False(t, demoProviders.Has("amazon-bedrock"))
}

func TestDemoBuildRuntimeUsesExplicitRegistry(t *testing.T) {
	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"primary":  {Provider: "anthropic", Model: "claude-sonnet-4-6"},
			"fallback": {Provider: "openai", Model: "gpt-4o-mini"},
		},
		Agents: latest.Agents{{Name: "root", Model: "primary", Fallback: &latest.FallbackConfig{Models: []string{"fallback"}}}},
	}
	rt, err := buildRuntime(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test", "OPENAI_API_KEY": "test"}), js.Undefined())
	require.NoError(t, err)
	assert.Contains(t, rt.providers, "root")
	require.Len(t, rt.fallbacks["root"], 1)
}
