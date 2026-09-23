package userid

import (
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGet_GeneratesAndPersistsUUID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := New(dir)

	id := r.Get()

	require.NotEmpty(t, id)
	parsed, err := uuid.Parse(id)
	require.NoError(t, err, "Get must return a valid UUID")
	assert.Equal(t, byte(4), parsed[6]>>4, "Get must generate a v4 UUID")
	assert.Equal(t, byte(0x80), parsed[8]&0xc0)

	data, err := os.ReadFile(filepath.Join(dir, fileName))
	require.NoError(t, err)
	assert.Equal(t, id, string(data), "Get must persist the UUID to disk")
}

func TestGet_ReturnsExistingUUID(t *testing.T) {
	t.Parallel()

	const canonical = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	for _, stored := range []string{
		canonical,
		"F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6",
		"f81d4fae7dec11d0a76500a0c91e6bf6",
		"{" + canonical + "}",
		"urn:uuid:" + canonical,
		"URN:UUID:" + canonical,
		"UrN:UuId:" + canonical,
		"[" + canonical + "]",
		"x" + canonical + "y",
	} {
		t.Run(stored, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			file := filepath.Join(dir, fileName)
			require.NoError(t, os.WriteFile(file, []byte(stored+"\n"), 0o600))

			assert.Equal(t, stored, New(dir).Get(), "Get must return the persisted UUID, trimmed")
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			assert.Equal(t, stored+"\n", string(data), "Get must not rewrite existing IDs")
		})
	}
}

func TestGet_RegeneratesOnEmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("   \n"), 0o600))

	id := New(dir).Get()
	require.NotEmpty(t, id)
	_, err := uuid.Parse(id)
	require.NoError(t, err, "Get must regenerate when the existing file is blank")
}

func TestGet_CachesAcrossCalls(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := New(dir)

	first := r.Get()

	// Mutating the file on disk after the first call must not change
	// the value returned by subsequent calls (it is served from the
	// in-memory cache).
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("changed-on-disk"), 0o600))

	assert.Equal(t, first, r.Get(), "Get must return the cached value on subsequent calls")
}

func TestGet_RegeneratesOnInvalidUUID(t *testing.T) {
	t.Parallel()

	for _, stored := range []string{
		"not-a-valid-uuid",
		"{f81d4fae-7dec-11d0-a765-00a0c91e6bfg}",
		"URN:UUID:f81d4fae-7dec-11d0-a765-00a0c91e6bfg",
		"bad:uuid:f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
	} {
		t.Run(stored, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			file := filepath.Join(dir, fileName)
			require.NoError(t, os.WriteFile(file, []byte(stored), 0o600))

			id := New(dir).Get()
			_, err := uuid.Parse(id)
			require.NoError(t, err, "Get must regenerate when the existing file contains an invalid UUID")
			assert.NotEqual(t, stored, id)

			data, err := os.ReadFile(file)
			require.NoError(t, err)
			assert.Equal(t, id, string(data), "Get must persist the regenerated UUID")
		})
	}
}
