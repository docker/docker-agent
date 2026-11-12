//go:build !js

package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
)

// TestCreateToolSet_DefaultPath_SanitisesReservedChars guards the
// default-path branch against config names that contain characters which
// are illegal in a Windows path component. Agents loaded from an OCI
// reference produce config names that include the image tag's ':', and
// without sanitisation os.MkdirAll fails with ERROR_INVALID_NAME on NTFS.
// The test runs the same assertions on every OS so the behaviour is
// pinned regardless of where the suite executes.
func TestCreateToolSet_DefaultPath_SanitisesReservedChars(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	paths.SetDataDir(dataDir)
	t.Cleanup(func() { paths.SetDataDir("") })

	ts, err := CreateToolSet(latest.Toolset{Type: "memory"}, "", &config.RuntimeConfig{}, `oci:v8<>"|?*\/-deadbeef`)
	require.NoError(t, err)
	require.NotNil(t, ts)

	// The directory under <dataDir>/memory must exist and contain no
	// Windows-reserved characters in its single path segment.
	entries, err := os.ReadDir(filepath.Join(dataDir, "memory"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	name := entries[0].Name()
	assert.NotContains(t, name, ":")
	assert.NotContains(t, name, "<")
	assert.NotContains(t, name, ">")
	assert.NotContains(t, name, `"`)
	assert.NotContains(t, name, "|")
	assert.NotContains(t, name, "?")
	assert.NotContains(t, name, "*")
	assert.NotContains(t, name, `\`)
	assert.NotContains(t, name, "/")
}

func TestDatabasePath(t *testing.T) {
	dataDir := t.TempDir()
	paths.SetDataDir(dataDir)
	t.Cleanup(func() { paths.SetDataDir("") })

	t.Run("default path per config", func(t *testing.T) {
		p, err := databasePath(latest.Toolset{Type: "memory"}, "", &config.RuntimeConfig{}, "my-agent")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dataDir, "memory", "my-agent", "memory.db"), p)
	})

	t.Run("empty config name falls back to default", func(t *testing.T) {
		p, err := databasePath(latest.Toolset{Type: "memory"}, "", &config.RuntimeConfig{}, "")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dataDir, "memory", "default", "memory.db"), p)
	})

	t.Run("reserved characters are sanitised", func(t *testing.T) {
		p, err := databasePath(latest.Toolset{Type: "memory"}, "", &config.RuntimeConfig{}, `oci:v8<>"|?*\/-deadbeef`)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dataDir, "memory", "oci_v8________-deadbeef", "memory.db"), p)
	})

	t.Run("explicit relative path resolves against parent dir", func(t *testing.T) {
		parentDir := t.TempDir()
		p, err := databasePath(latest.Toolset{Type: "memory", Path: "state/mem.db"}, parentDir, &config.RuntimeConfig{}, "ignored")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(parentDir, "state", "mem.db"), p)
	})

	t.Run("explicit path escaping parent dir is rejected", func(t *testing.T) {
		_, err := databasePath(latest.Toolset{Type: "memory", Path: "../escape.db"}, t.TempDir(), &config.RuntimeConfig{}, "ignored")
		require.ErrorContains(t, err, "invalid memory database path")
	})
}
