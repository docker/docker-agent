package userid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestGet_GeneratesAndPersistsUUID(t *testing.T) {
	dir := t.TempDir()
	useConfigDir(t, dir)

	id := Get()

	require.NotEmpty(t, id)
	_, err := uuid.Parse(id)
	require.NoError(t, err, "Get must return a valid UUID")

	data, err := os.ReadFile(filepath.Join(dir, fileName))
	require.NoError(t, err)
	assert.Equal(t, id, string(data), "Get must persist the UUID to disk")
}

func TestGet_ReturnsExistingUUID(t *testing.T) {
	dir := t.TempDir()
	useConfigDir(t, dir)

	const stored = "11111111-2222-3333-4444-555555555555"
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte(stored+"\n"), 0o600))

	assert.Equal(t, stored, Get(), "Get must return the persisted UUID, trimmed")
}

func TestGet_RegeneratesOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	useConfigDir(t, dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("   \n"), 0o600))

	id := Get()
	require.NotEmpty(t, id)
	_, err := uuid.Parse(id)
	require.NoError(t, err, "Get must regenerate when the existing file is blank")
}

func TestGet_CachesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	useConfigDir(t, dir)

	first := Get()

	// Mutating the file on disk after the first call must not change
	// the value returned by subsequent calls (it is served from the
	// in-memory cache).
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("changed-on-disk"), 0o600))

	assert.Equal(t, first, Get(), "Get must return the cached value on subsequent calls")
}

// useConfigDir points paths.GetConfigDir at dir for the duration of the
// test and resets the in-memory cache so [Get] is forced to re-read
// from disk. The override is removed and the cache is cleared on
// cleanup so subsequent tests start fresh.
//
// These tests intentionally do not call [t.Parallel] because they
// share package-level mutable state (the cached UUID and the global
// config-dir override).
func useConfigDir(t *testing.T, dir string) {
	t.Helper()

	paths.SetConfigDir(dir)
	ResetForTests()

	t.Cleanup(func() {
		paths.SetConfigDir("")
		ResetForTests()
	})
}

func TestGet_RegeneratesOnInvalidUUID(t *testing.T) {
	dir := t.TempDir()
	useConfigDir(t, dir)

	// Write an invalid UUID to the file (e.g., manually corrupted)
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("not-a-valid-uuid"), 0o600))

	id := Get()
	require.NotEmpty(t, id)
	_, err := uuid.Parse(id)
	require.NoError(t, err, "Get must regenerate when the existing file contains an invalid UUID")

	// Verify the new valid UUID was persisted
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	require.NoError(t, err)
	assert.Equal(t, id, string(data), "Get must persist the regenerated UUID")
}
