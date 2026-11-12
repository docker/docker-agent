package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/paths"
)

func TestEnvProviderOverride(t *testing.T) {
	t.Parallel()
	env := environment.NewMapEnvProvider(map[string]string{"TOKEN": "session-secret"})
	cfg := &RuntimeConfig{
		EnvProviderOverride:    env,
		EnvProviderForTests:    environment.NewMapEnvProvider(map[string]string{"TOKEN": "legacy"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot()),
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

func TestCloneWithFreshEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "credentials.env")
	require.NoError(t, os.WriteFile(path, []byte("ACP_REFRESH_KEY=old\n"), 0o600))
	rc := &RuntimeConfig{Config: Config{EnvFiles: []string{path}}, ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot())}
	original := rc.EnvProvider()
	require.NoError(t, os.WriteFile(path, []byte("ACP_REFRESH_KEY=new\n"), 0o600))
	fresh := rc.CloneWithFreshEnvironment()
	value, ok := fresh.EnvProvider().Get(t.Context(), "ACP_REFRESH_KEY")
	require.True(t, ok)
	assert.Equal(t, "new", value)
	value, ok = original.Get(t.Context(), "ACP_REFRESH_KEY")
	require.True(t, ok)
	assert.Equal(t, "old", value)
	assert.Same(t, original, rc.EnvProvider())
	assert.Same(t, rc.ModelsDevStoreOverride, fresh.ModelsDevStoreOverride)
}

func TestCloneWithFreshEnvironmentErrorsAndOverrides(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"invalid path", "missing file", "malformed file", "override", "legacy override"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.env")
			rc := &RuntimeConfig{Config: Config{EnvFiles: []string{path}}, ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot())}
			switch kind {
			case "invalid path":
				rc.EnvFiles = []string{"~unsupported/path"}
			case "malformed file":
				require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o600))
			case "override":
				rc.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{"KEY": "value"})
			case "legacy override":
				rc.EnvProviderForTests = environment.NewMapEnvProvider(map[string]string{"KEY": "value"})
			}
			original := rc.EnvProvider()
			fresh := rc.CloneWithFreshEnvironment()
			if strings.Contains(kind, "override") {
				require.NoError(t, fresh.EnvFilesError())
				assert.Same(t, original, fresh.EnvProvider())
			} else {
				require.Error(t, rc.EnvFilesError())
				require.Error(t, fresh.EnvFilesError())
				assert.NotEmpty(t, fresh.EnvFiles)
				if kind != "invalid path" {
					require.NoError(t, os.WriteFile(path, []byte("KEY=repaired\n"), 0o600))
					repaired := rc.CloneWithFreshEnvironment()
					require.NoError(t, repaired.EnvFilesError())
					value, ok := repaired.EnvProvider().Get(t.Context(), "KEY")
					require.True(t, ok)
					assert.Equal(t, "repaired", value)
				}
			}
		})
	}
}

func TestCloneWithFreshEnvironmentNewDefaultFile(t *testing.T) {
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	t.Setenv("ACP_REFRESH_PRECEDENCE", "environment")
	rc := &RuntimeConfig{ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot())}
	original := rc.EnvProvider()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("ACP_REFRESH_NEW_KEY=file\nACP_REFRESH_PRECEDENCE=file\n"), 0o600))
	fresh := rc.CloneWithFreshEnvironment()
	value, found := fresh.EnvProvider().Get(t.Context(), "ACP_REFRESH_NEW_KEY")
	require.True(t, found)
	assert.Equal(t, "file", value)
	_, found = original.Get(t.Context(), "ACP_REFRESH_NEW_KEY")
	assert.False(t, found)
	value, found = fresh.EnvProvider().Get(t.Context(), "ACP_REFRESH_PRECEDENCE")
	require.True(t, found)
	assert.Equal(t, "environment", value)
	explicit := filepath.Join(t.TempDir(), "explicit.env")
	require.NoError(t, os.WriteFile(explicit, []byte("ACP_REFRESH_PRECEDENCE=explicit\nACP_REFRESH_PRECEDENCE=duplicate\nACP_REFRESH_EMPTY=\n"), 0o600))
	rc.EnvFiles = []string{explicit}
	fresh = rc.CloneWithFreshEnvironment()
	value, found = fresh.EnvProvider().Get(t.Context(), "ACP_REFRESH_PRECEDENCE")
	require.True(t, found)
	assert.Equal(t, "explicit", value)
	value, found = fresh.EnvProvider().Get(t.Context(), "ACP_REFRESH_EMPTY")
	require.True(t, found)
	assert.Empty(t, value)
}
