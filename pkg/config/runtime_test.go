package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
)

func TestClone_ChangeWorkingDir(t *testing.T) {
	t.Parallel()
	original := &RuntimeConfig{
		Config: Config{
			EnvFiles:       []string{"file1.env", "file2.env"},
			ModelsGateway:  "http://models.gateway",
			GlobalCodeMode: true,
			WorkingDir:     "/app",
		},
	}

	clone := original.Clone()
	original.WorkingDir = "/newapp"
	clone.WorkingDir = "/cloneapp"

	assert.Equal(t, "/newapp", original.WorkingDir)
	assert.Equal(t, "/cloneapp", clone.WorkingDir)
}

func TestEnvProvider_EnvOverrides(t *testing.T) {
	t.Parallel()

	t.Run("overrides take precedence over the computed chain", func(t *testing.T) {
		t.Parallel()
		rc := &RuntimeConfig{
			EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"FOO": "real"}),
			Config:              Config{EnvOverrides: map[string]string{"FOO": "override"}},
		}

		// EnvProviderForTests wins outright: EnvOverrides only layers onto the
		// computed production chain, not a test double.
		v, ok := rc.EnvProvider().Get(t.Context(), "FOO")
		require.True(t, ok)
		assert.Equal(t, "real", v)
	})

	t.Run("overrides layer onto the computed chain without replacing it", func(t *testing.T) {
		t.Parallel()
		ok := filepath.Join(t.TempDir(), "ok.env")
		require.NoError(t, os.WriteFile(ok, []byte("SOME_TEST_ONLY_VAR=from-env-file\n"), 0o600))
		rc := &RuntimeConfig{Config: Config{
			EnvFiles:     []string{ok},
			EnvOverrides: map[string]string{"SOME_OTHER_VAR": "overridden"},
		}}

		v, found := rc.EnvProvider().Get(t.Context(), "SOME_OTHER_VAR")
		require.True(t, found)
		assert.Equal(t, "overridden", v)

		// Values not present in EnvOverrides still fall through to the rest
		// of the chain (env files here).
		v, found = rc.EnvProvider().Get(t.Context(), "SOME_TEST_ONLY_VAR")
		require.True(t, found)
		assert.Equal(t, "from-env-file", v)
	})

	t.Run("set after the chain was first resolved still applies", func(t *testing.T) {
		t.Parallel()
		rc := &RuntimeConfig{}

		// Simulate flag parsing materializing the chain before recording mode
		// (which sets EnvOverrides later) runs. Use a var name that can't
		// already be set in the ambient environment.
		const varName = "CAGENT_TEST_ENV_PROVIDER_OVERRIDE_LATE_SET"
		_, found := rc.EnvProvider().Get(t.Context(), varName)
		assert.False(t, found)

		rc.EnvOverrides = map[string]string{varName: "placeholder"}

		v, found := rc.EnvProvider().Get(t.Context(), varName)
		require.True(t, found)
		assert.Equal(t, "placeholder", v)
	})

	t.Run("clone gets an independent copy", func(t *testing.T) {
		t.Parallel()
		rc := &RuntimeConfig{Config: Config{EnvOverrides: map[string]string{"FOO": "bar"}}}

		clone := rc.Clone()
		clone.EnvOverrides["FOO"] = "mutated"

		assert.Equal(t, "bar", rc.EnvOverrides["FOO"], "mutating the clone's map must not affect the original")
	})
}

func TestEnvFilesError(t *testing.T) {
	t.Parallel()

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing.env")
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{missing}}}

		err := rc.EnvFilesError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing.env")
	})

	t.Run("malformed file", func(t *testing.T) {
		t.Parallel()
		bad := filepath.Join(t.TempDir(), "bad.env")
		require.NoError(t, os.WriteFile(bad, []byte("NOT_A_PAIR\n"), 0o600))
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{bad}}}

		err := rc.EnvFilesError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad.env")
	})

	t.Run("valid file", func(t *testing.T) {
		t.Parallel()
		ok := filepath.Join(t.TempDir(), "ok.env")
		require.NoError(t, os.WriteFile(ok, []byte("SOME_TEST_ONLY_VAR=some-value\n"), 0o600))
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{ok}}}

		require.NoError(t, rc.EnvFilesError())
		v, _ := rc.EnvProvider().Get(t.Context(), "SOME_TEST_ONLY_VAR")
		assert.Equal(t, "some-value", v)
	})

	t.Run("clone preserves error", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing.env")
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{missing}}}

		require.Error(t, rc.Clone().EnvFilesError())
	})
}
