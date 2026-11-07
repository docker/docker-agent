package logging

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotatingFile_Write(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rf, err := NewRotatingFile(path, withMaxSize(100), withMaxBackups(2))
	require.NoError(t, err)
	defer rf.Close()

	data := []byte("hello world\n")
	n, err := rf.Write(data)
	require.NoError(t, err)
	assert.Equal(t, len(data), n)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, content)
}

func TestRotatingFile_Rotate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rf, err := NewRotatingFile(path, withMaxSize(50), withMaxBackups(2))
	require.NoError(t, err)
	defer rf.Close()

	// Write enough data to trigger rotation
	data := make([]byte, 30)
	for i := range data {
		data[i] = 'a'
	}

	_, err = rf.Write(data)
	require.NoError(t, err)

	// This write should trigger rotation
	_, err = rf.Write(data)
	require.NoError(t, err)

	// Check that backup file was created
	_, err = os.Stat(path + ".1")
	require.NoError(t, err, "backup file should exist")

	// Original file should have new content
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, content)

	// Backup should have old content
	backup, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	assert.Equal(t, data, backup)
}

func TestRotatingFile_MaxBackups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rf, err := NewRotatingFile(path, withMaxSize(20), withMaxBackups(2))
	require.NoError(t, err)
	defer rf.Close()

	data := make([]byte, 15)

	// Write 4 times to trigger multiple rotations
	for i := range 4 {
		for j := range data {
			data[j] = byte('a' + i)
		}
		_, err = rf.Write(data)
		require.NoError(t, err)
	}

	// Should have current file + 2 backups (maxBackups=2)
	_, err = os.Stat(path)
	require.NoError(t, err, "current file should exist")

	_, err = os.Stat(path + ".1")
	require.NoError(t, err, "backup .1 should exist")

	_, err = os.Stat(path + ".2")
	require.NoError(t, err, "backup .2 should exist")

	// .3 should NOT exist because maxBackups=2
	_, err = os.Stat(path + ".3")
	require.True(t, os.IsNotExist(err), "backup .3 should not exist")
}

func TestRotatingFile_AppendsToExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Create a file with existing content
	err := os.WriteFile(path, []byte("existing\n"), 0o600)
	require.NoError(t, err)

	rf, err := NewRotatingFile(path, withMaxSize(1000), withMaxBackups(2))
	require.NoError(t, err)
	defer rf.Close()

	_, err = rf.Write([]byte("new\n"))
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "existing\nnew\n", string(content))
}

func TestRotatingFile_CreatesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "nested", "test.log")

	rf, err := NewRotatingFile(path)
	require.NoError(t, err)
	defer rf.Close()

	_, err = rf.Write([]byte("test"))
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)
}

func withMaxSize(size int64) Option {
	return func(r *RotatingFile) {
		r.maxSize = size
	}
}

func withMaxBackups(count int) Option {
	return func(r *RotatingFile) {
		r.maxBackups = count
	}
}
