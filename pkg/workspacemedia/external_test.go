package workspacemedia

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteExternal_WritesConfirmedTarget(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "exports", "cat.png")

	res, err := WriteExternal(target, []byte{0x01, 0x02}, "image/png")
	require.NoError(t, err)
	assert.Equal(t, target, res.RelPath)
	assert.False(t, res.ExtensionCorrected)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x01, 0x02}, data)

	entries, err := os.ReadDir(filepath.Dir(target))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file may survive the atomic publish")
}

func TestWriteExternal_NeverOverwrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "cat.png")
	require.NoError(t, os.WriteFile(target, []byte("existing"), 0o644))

	res, err := WriteExternal(target, []byte("new"), "image/png")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "cat-1.png"), res.RelPath, "an existing file must get a dash suffix")

	existing, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("existing"), existing, "the pre-existing file must be untouched")

	written, err := os.ReadFile(res.RelPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), written)
}

func TestWriteExternal_CorrectsExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	res, err := WriteExternal(filepath.Join(dir, "cat.txt"), []byte{0x01}, "image/png")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "cat.png"), res.RelPath)
	assert.True(t, res.ExtensionCorrected)
	assert.Equal(t, ".txt", res.RequestedExtension)
}

func TestWriteExternal_RejectsUnconfirmableTargets(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"relative/cat.png",
		"cat.png",
		"",
		"/abs/../cat.png",
		"/",
	} {
		_, err := WriteExternal(target, []byte{0x01}, "image/png")
		require.ErrorIs(t, err, ErrPathEscape, "target %q must be rejected", target)
	}
}

func TestWriteExternal_FailedWriteLeavesNoClaim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "cat.png")

	// An unreadable source is not injectable through the []byte API, so
	// provoke the failure via the reader-level implementation.
	_, err := writeExternal(target, failingReader{}, "image/png")
	require.Error(t, err)

	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "a failed write must not leave an empty claim behind")
}

func TestWriteExternal_RejectsExistingDirectoryTarget(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	target := filepath.Join(parent, "exports")
	require.NoError(t, os.Mkdir(target, 0o755))

	_, err := WriteExternal(target, []byte{0x01}, "image/png")
	require.ErrorIs(t, err, ErrPathEscape, "a directory at the confirmed path must be rejected, not dash-suffixed")

	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no sibling file may appear next to the directory")
	inside, err := os.ReadDir(target)
	require.NoError(t, err)
	assert.Empty(t, inside, "nothing may be written inside the directory without a confirmed file path")
}
