package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_disabled(t *testing.T) {
	c, err := New(Config{Enabled: false})
	require.NoError(t, err)
	assert.Nil(t, c)
}

func TestMemoryCache_caseSensitiveDefault(t *testing.T) {
	c, err := New(Config{Enabled: true, CaseSensitive: true})
	require.NoError(t, err)
	require.NotNil(t, c)

	c.Store("Hello", "world")

	got, ok := c.Lookup("Hello")
	assert.True(t, ok)
	assert.Equal(t, "world", got)

	_, ok = c.Lookup("hello")
	assert.False(t, ok, "case-sensitive cache should not match different case")
}

func TestMemoryCache_caseInsensitive(t *testing.T) {
	c, err := New(Config{Enabled: true, CaseSensitive: false})
	require.NoError(t, err)

	c.Store("Hello", "world")

	got, ok := c.Lookup("HELLO")
	assert.True(t, ok)
	assert.Equal(t, "world", got)
}

func TestMemoryCache_trimSpaces(t *testing.T) {
	c, err := New(Config{Enabled: true, TrimSpaces: true})
	require.NoError(t, err)

	c.Store("  hello  ", "world")

	got, ok := c.Lookup("hello")
	assert.True(t, ok)
	assert.Equal(t, "world", got)

	got, ok = c.Lookup("\thello\n")
	assert.True(t, ok)
	assert.Equal(t, "world", got)
}

func TestMemoryCache_noTrimByDefault(t *testing.T) {
	c, err := New(Config{Enabled: true})
	require.NoError(t, err)

	c.Store("  hello  ", "world")

	_, ok := c.Lookup("hello")
	assert.False(t, ok, "without TrimSpaces, whitespace must be significant")
}

func TestMemoryCache_overwrite(t *testing.T) {
	c, err := New(Config{Enabled: true})
	require.NoError(t, err)

	c.Store("q", "first")
	c.Store("q", "second")

	got, ok := c.Lookup("q")
	assert.True(t, ok)
	assert.Equal(t, "second", got)
}

// TestFileCache_dedupSkipsRedundantWrite verifies that storing the exact
// same (question, response) pair twice is treated as a no-op, so the
// underlying JSON file is rewritten only on the first Store. This is
// what keeps cache replays free of redundant disk traffic.
func TestFileCache_dedupSkipsRedundantWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	c.Store("q", "a")
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)

	// Same pair: must not rewrite the file (mtime stays the same).
	c.Store("q", "a")
	infoAfter, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, infoBefore.ModTime(), infoAfter.ModTime(),
		"identical Store must not rewrite the cache file")

	// Different value: must rewrite.
	c.Store("q", "b")
	infoChanged, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, infoChanged.ModTime().After(infoBefore.ModTime()) || infoChanged.Size() != infoBefore.Size(),
		"different Store must rewrite the cache file")
}

func TestFileCache_persists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c1, err := New(Config{Enabled: true, Path: path, CaseSensitive: false, TrimSpaces: true})
	require.NoError(t, err)
	c1.Store("  Hello  ", "world")

	// File must exist on disk and contain the normalized key.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var entries map[string]string
	require.NoError(t, json.Unmarshal(data, &entries))
	assert.Equal(t, map[string]string{"hello": "world"}, entries)

	// A new cache loaded from the same file recovers the entries.
	c2, err := New(Config{Enabled: true, Path: path, CaseSensitive: false, TrimSpaces: true})
	require.NoError(t, err)
	got, ok := c2.Lookup("HELLO")
	assert.True(t, ok)
	assert.Equal(t, "world", got)
}

func TestFileCache_missingFileIsFine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "cache.json")

	c, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	_, ok := c.Lookup("anything")
	assert.False(t, ok)

	c.Store("hello", "world")

	got, ok := c.Lookup("hello")
	assert.True(t, ok)
	assert.Equal(t, "world", got)

	// And the directory should have been created.
	_, err = os.Stat(path)
	assert.NoError(t, err)
}

func TestFileCache_corruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))

	_, err := New(Config{Enabled: true, Path: path})
	assert.Error(t, err)
}

// TestFileCache_persistenceFailureKeepsInMemory verifies that a Store
// whose underlying file write fails still updates the in-memory map: a
// transient disk error must not break the running agent's turn, and a
// subsequent Lookup must return the value the caller just wrote.
//
// We force a write failure by stripping write permission from the cache
// directory after [New] returned. The persist callback (running inside
// Store) tries os.CreateTemp in that directory and fails; the error is
// swallowed and the cache keeps serving from memory.
func TestFileCache_persistenceFailureKeepsInMemory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are bypassed, can't force a write failure")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	// Strip write permission on the directory; restored before t.TempDir
	// cleanup runs so the dir can be removed.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Store must not panic or block on the underlying write failure.
	c.Store("q", "a")

	// In-memory state is intact even though the file write failed.
	got, ok := c.Lookup("q")
	assert.True(t, ok)
	assert.Equal(t, "a", got)

	// And the file was indeed never written.
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist,
		"cache file must not exist when the parent directory is read-only")
}

// TestFileCache_atomicWriteLeavesNoTempFiles verifies that the rename-based
// atomic write does not leak temporary files on the happy path. Only the
// JSON file and the persistent .lock sentinel are expected to remain.
func TestFileCache_atomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	for i := range 5 {
		c.Store(fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	expected := map[string]bool{
		"cache.json":      true, // the data file
		"cache.json.lock": true, // the cross-process advisory lock
	}
	for _, e := range entries {
		assert.True(t, expected[e.Name()],
			"unexpected leftover in cache directory: %q", e.Name())
	}
}

// TestFileCache_concurrentStoreNeverYieldsTornFile verifies that concurrent
// Store calls always leave a fully valid JSON file behind — i.e. a parallel
// reader will never observe a half-written cache thanks to the
// rename-over-temp atomicity.
func TestFileCache_concurrentStoreNeverYieldsTornFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 50 {
			c.Store(fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
		}
	}()

	// While writes are happening, repeatedly read and parse the file.
	// Without atomic rename, this would intermittently see truncated /
	// half-written content and json.Unmarshal would error.
	for range 100 {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		require.NoError(t, err)
		if len(data) == 0 {
			continue
		}
		var m map[string]string
		require.NoError(t, json.Unmarshal(data, &m),
			"reader observed a torn write: %q", string(data))
	}

	<-done
}

// TestFileCache_crossProcessConcurrentStoresPreserveAllEntries simulates
// two independent processes (each with its own *Cache instance pointed
// at the same path) racing to Store many distinct keys. The advisory
// file lock taken by Cache.Store must serialize the read-modify-write
// window so that — after both finish — the on-disk file contains every
// entry written by either side. Without the lock, each side would read
// a stale snapshot under its own mutex, and the last writer would
// silently clobber the other's entries.
func TestFileCache_crossProcessConcurrentStoresPreserveAllEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	cA, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)
	cB, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	const writesPerSide = 50
	doneA := make(chan struct{})
	doneB := make(chan struct{})

	go func() {
		defer close(doneA)
		for i := range writesPerSide {
			cA.Store(fmt.Sprintf("a%d", i), fmt.Sprintf("va%d", i))
		}
	}()
	go func() {
		defer close(doneB)
		for i := range writesPerSide {
			cB.Store(fmt.Sprintf("b%d", i), fmt.Sprintf("vb%d", i))
		}
	}()

	<-doneA
	<-doneB

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var onDisk map[string]string
	require.NoError(t, json.Unmarshal(data, &onDisk))

	assert.Lenf(t, onDisk, 2*writesPerSide,
		"expected every key from both processes to be on disk; got %d", len(onDisk))
	for i := range writesPerSide {
		assert.Equal(t, fmt.Sprintf("va%d", i), onDisk[fmt.Sprintf("a%d", i)])
		assert.Equal(t, fmt.Sprintf("vb%d", i), onDisk[fmt.Sprintf("b%d", i)])
	}
}

// TestFileCache_lookupReloadsAfterExternalWrite verifies that a Cache
// instance picks up entries written by another instance (read: another
// process) without restart: when Lookup notices the file mtime has
// advanced since the last load, it re-reads the file and the new entry
// becomes visible.
func TestFileCache_lookupReloadsAfterExternalWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	reader, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	// Initially nothing on disk; reader sees no entry.
	_, ok := reader.Lookup("q")
	require.False(t, ok)

	writer, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)
	writer.Store("q", "a")

	// File mtime granularity on some filesystems (macOS HFS+ historically,
	// some network FS) is 1 second. Bump the mtime explicitly so we don't
	// false-pass on a coincidentally-equal timestamp. We use a future
	// time so the reader's last-seen mtime is unambiguously older.
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	got, ok := reader.Lookup("q")
	assert.True(t, ok, "reader must reload and see the entry written by the sibling instance")
	assert.Equal(t, "a", got)
}

// TestFileCache_lockFileNeverDeleted asserts that the .lock sidecar is
// not removed by Store, since deleting it could let two concurrent
// processes lock different inodes and lose mutual exclusion. The lock
// file is a long-lived sentinel.
func TestFileCache_lockFileNeverDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c, err := New(Config{Enabled: true, Path: path})
	require.NoError(t, err)

	c.Store("q", "a")
	info1, err := os.Stat(path + ".lock")
	require.NoError(t, err, "lock file must exist after first Store")

	c.Store("q2", "a2")
	info2, err := os.Stat(path + ".lock")
	require.NoError(t, err, "lock file must persist across Stores")
	assert.Equal(t, info1.Sys(), info2.Sys(),
		"lock file inode must be stable across Stores so flock semantics hold")
}
