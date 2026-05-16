package atomicfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

func TestWriteCreatesFileWithMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are POSIX-only")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	require.NoError(t, atomicfile.Write(path, bytes.NewReader([]byte("hello")), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteOverwritesAndRetightensMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are POSIX-only")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, atomicfile.Write(path, bytes.NewReader([]byte("new")), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteReturnsErrorForMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "file")

	err := atomicfile.Write(path, bytes.NewReader([]byte("x")), 0o600)
	assert.Error(t, err)
}
