package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClone_ChangeWorkingDir(t *testing.T) {
	t.Parallel()
	original := &RuntimeConfig{
		EnvFiles:       []string{"file1.env", "file2.env"},
		ModelsGateway:  "http://models.gateway",
		GlobalCodeMode: true,
		WorkingDir:     "/app",
	}

	clone := original.Clone()
	original.WorkingDir = "/newapp"
	clone.WorkingDir = "/cloneapp"

	assert.Equal(t, "/newapp", original.WorkingDir)
	assert.Equal(t, "/cloneapp", clone.WorkingDir)
}

func TestEnvFilesError(t *testing.T) {
	t.Parallel()

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing.env")
		rc := &RuntimeConfig{EnvFiles: []string{missing}}

		err := rc.EnvFilesError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing.env")
	})

	t.Run("malformed file", func(t *testing.T) {
		t.Parallel()
		bad := filepath.Join(t.TempDir(), "bad.env")
		require.NoError(t, os.WriteFile(bad, []byte("NOT_A_PAIR\n"), 0o600))
		rc := &RuntimeConfig{EnvFiles: []string{bad}}

		err := rc.EnvFilesError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad.env")
	})

	t.Run("valid file", func(t *testing.T) {
		t.Parallel()
		ok := filepath.Join(t.TempDir(), "ok.env")
		require.NoError(t, os.WriteFile(ok, []byte("SOME_TEST_ONLY_VAR=some-value\n"), 0o600))
		rc := &RuntimeConfig{EnvFiles: []string{ok}}

		require.NoError(t, rc.EnvFilesError())
		v, _ := rc.EnvProvider().Get(t.Context(), "SOME_TEST_ONLY_VAR")
		assert.Equal(t, "some-value", v)
	})

	t.Run("clone preserves error", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing.env")
		rc := &RuntimeConfig{EnvFiles: []string{missing}}

		require.Error(t, rc.Clone().EnvFilesError())
	})
}
