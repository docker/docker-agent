package atomicfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteCreatesFileWithMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are POSIX-only")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	require.NoError(t, Write(path, bytes.NewReader([]byte("hello")), 0o640))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestWriteOverwritesAndRetightensMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are POSIX-only")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, Write(path, bytes.NewReader([]byte("new")), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteReturnsErrorForMissingDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "file")

	err := Write(path, bytes.NewReader([]byte("x")), 0o600)
	assert.Error(t, err)
}

type partialFailReader struct {
	chunk []byte
	err   error
}

func (r *partialFailReader) Read(p []byte) (int, error) {
	if len(r.chunk) > 0 {
		n := copy(p, r.chunk)
		r.chunk = r.chunk[n:]
		return n, nil
	}
	return 0, r.err
}

func TestWrite_CleansUpTempFileOnMidWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "artifact.bin")
	injectedErr := errors.New("injected mid-write failure")

	err := Write(target, &partialFailReader{chunk: []byte("partial-data"), err: injectedErr}, 0o600)
	require.Error(t, err)
	assert.Contains(t, err.Error(), injectedErr.Error())

	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "the destination file must never be created on a mid-write failure")

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no partial/orphan temp file must remain in the directory after a mid-write failure")
}

func TestWrite_CleansUpTempFileOnMidWriteFailure_ExistingDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "artifact.bin")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o600))
	injectedErr := errors.New("injected mid-write failure")

	err := Write(target, &partialFailReader{chunk: []byte("partial-data"), err: injectedErr}, 0o600)
	require.Error(t, err)
	assert.Contains(t, err.Error(), injectedErr.Error())

	data, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "original", string(data), "the original destination content must survive a failed replacement")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no partial/orphan temp file must remain alongside the untouched destination")
	assert.Equal(t, "artifact.bin", entries[0].Name())
}

func TestPartialFailReaderAcrossSmallBuffers(t *testing.T) {
	t.Parallel()
	injectedErr := errors.New("injected read failure")
	r := &partialFailReader{chunk: []byte("abc"), err: injectedErr}
	buf := make([]byte, 2)
	for _, want := range []string{"ab", "c"} {
		n, err := r.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, want, string(buf[:n]))
	}
	n, err := r.Read(buf)
	assert.Zero(t, n)
	assert.ErrorIs(t, err, injectedErr)
}

func TestWrite_PublishFailureCleansUpTemporaryFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "destination")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(target, "existing"), []byte("keep"), 0o600))

	require.Error(t, Write(target, bytes.NewReader([]byte("replacement")), 0o600))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "destination", entries[0].Name())
	data, err := os.ReadFile(filepath.Join(target, "existing"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
}
