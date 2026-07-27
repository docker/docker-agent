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

// TestWriteCreatesFileWithMode is the plan's restored "new file gets the
// requested mode" regression, inadvertently dropped when this file was
// rewritten to add the mid-write cleanup tests below: Write must create a
// brand-new file with exactly the requested permission bits, not whatever
// natefinch/atomic.WriteFile's own default (umask-derived) mode would be.
func TestWriteCreatesFileWithMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are POSIX-only")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	require.NoError(t, Write(path, bytes.NewReader([]byte("hello")), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestWriteOverwritesAndRetightensMode is the restored counterpart for an
// EXISTING destination with a looser mode (0o644): a replacement Write
// must retighten it to exactly the requested (0o600) bits, not merely
// preserve whatever mode the file already had.
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

// TestWriteReturnsErrorForMissingDirectory is the restored regression for
// a missing parent directory: Write must surface the underlying error
// rather than creating the parent itself or panicking.
func TestWriteReturnsErrorForMissingDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "file")

	err := Write(path, bytes.NewReader([]byte("x")), 0o600)
	assert.Error(t, err)
}

// partialFailReader returns chunk on its first Read (so the underlying
// os.CreateTemp'd file genuinely receives that data before anything goes
// wrong) and then fails on every subsequent Read. This reproduces a
// mid-write failure AFTER a real, partially written temporary file
// already exists on disk — the seam the plan requires ("deterministically
// fail only after a controlled temporary/partial output has been
// created"), as opposed to a permission trick that would prevent the
// temp file from ever being created at all.
type partialFailReader struct {
	chunk []byte
	err   error
	sent  bool
}

func (r *partialFailReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.chunk)
		return n, nil
	}
	return 0, r.err
}

// TestWrite_CleansUpTempFileOnMidWriteFailure proves the real production
// cleanup path: after Write's underlying temp file has already received
// partial data, a subsequent read failure must leave no temp file behind
// in the target directory, and the destination path itself must remain
// untouched (never partially written, never created if it didn't already
// exist).
func TestWrite_CleansUpTempFileOnMidWriteFailure(t *testing.T) {
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

// TestWrite_CleansUpTempFileOnMidWriteFailure_ExistingDestination is the
// same regression when the destination already has prior content: a
// mid-write failure for a REPLACEMENT write must leave the original
// content intact and, again, no temp file residue.
func TestWrite_CleansUpTempFileOnMidWriteFailure_ExistingDestination(t *testing.T) {
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
