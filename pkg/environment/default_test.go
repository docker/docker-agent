package environment

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultSources(t *testing.T) {
	// Not parallel: DefaultSources reads the real user config, which is cheap
	// but environment-dependent.
	sources := DefaultSources()

	names := make([]string, 0, len(sources))
	for _, source := range sources {
		require.NotNil(t, source.Provider, "source %q has a nil provider", source.Name)
		names = append(names, source.Name)
	}

	// Names must be unique so diagnostic output can identify each source.
	sorted := slices.Sorted(slices.Values(names))
	assert.Len(t, slices.Compact(sorted), len(names))

	// The core sources are always present and keep their precedence order.
	envIdx := slices.Index(names, "environment")
	secretsIdx := slices.Index(names, "run-secrets")
	desktopIdx := slices.Index(names, "docker-desktop")
	require.GreaterOrEqual(t, envIdx, 0)
	require.Greater(t, secretsIdx, envIdx)
	require.Greater(t, desktopIdx, secretsIdx)
}

func TestNewDefaultProvider_UsesDefaultSources(t *testing.T) {
	t.Setenv("SOME_DOCKER_AGENT_TEST_ONLY_VAR", "value")

	provider := NewDefaultProvider()

	value, found := provider.Get(t.Context(), "SOME_DOCKER_AGENT_TEST_ONLY_VAR")
	require.True(t, found)
	assert.Equal(t, "value", value)
}

func TestNewDefaultProvider_PrefixedNameWinsAcrossSources(t *testing.T) {
	dir := withTempConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("DOCKER_AGENT_ENV_MY_TEST_KEY=from-config-env\n"), 0o600))
	t.Setenv("MY_TEST_KEY", "from-shell")

	value, found := NewDefaultProvider().Get(t.Context(), "MY_TEST_KEY")
	require.True(t, found)
	assert.Equal(t, "from-config-env", value)
}

func defaultSourceNames() []string {
	sources := DefaultSources()
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return names
}

func TestDefaultSources_WithoutConfigEnvFile(t *testing.T) {
	withTempConfigDir(t)

	assert.NotContains(t, defaultSourceNames(), "config-env-file")
}

func TestDefaultSources_ReadsConfigEnvFile(t *testing.T) {
	dir := withTempConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("MY_TEST_KEY=from-config-env\n"), 0o600))

	sources := DefaultSources()

	names := defaultSourceNames()
	configIdx := slices.Index(names, "config-env-file")
	require.GreaterOrEqual(t, configIdx, 0)

	// The file sits below explicit sources (OS env, run secrets) and above
	// Docker Desktop in lookup precedence.
	assert.Greater(t, configIdx, slices.Index(names, "run-secrets"))
	assert.Less(t, configIdx, slices.Index(names, "docker-desktop"))

	value, found := sources[configIdx].Provider.Get(t.Context(), "MY_TEST_KEY")
	require.True(t, found)
	assert.Equal(t, "from-config-env", value)
}

func TestDefaultSources_SkipsMalformedConfigEnvFile(t *testing.T) {
	dir := withTempConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("not a key value line\n"), 0o600))

	// A stray edit must not lock every command out of the default chain.
	assert.NotContains(t, defaultSourceNames(), "config-env-file")
}
