package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestEnvProviderOverride(t *testing.T) {
	t.Parallel()
	env := environment.NewMapEnvProvider(map[string]string{"TOKEN": "session-secret"})
	cfg := &RuntimeConfig{
		EnvProviderOverride:    env,
		EnvProviderForTests:    environment.NewMapEnvProvider(map[string]string{"TOKEN": "legacy"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(modelsdev.EmbeddedSnapshot()),
		EnvFiles:               []string{"/does-not-exist.env"},
	}
	for _, c := range []*RuntimeConfig{cfg, cfg.Clone()} {
		require.NoError(t, c.EnvFilesError())
		value, ok := c.EnvProvider().Get(t.Context(), "TOKEN")
		require.True(t, ok)
		assert.Equal(t, "session-secret", value)
	}
}

func TestLegacyEnvProviderOverride(t *testing.T) {
	t.Parallel()
	env := environment.NewMapEnvProvider(map[string]string{"TOKEN": "legacy"})
	cfg := &RuntimeConfig{EnvProviderForTests: env}
	value, ok := cfg.EnvProvider().Get(t.Context(), "TOKEN")
	require.True(t, ok)
	assert.Equal(t, "legacy", value)
}
