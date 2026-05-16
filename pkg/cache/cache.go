// Package cache implements a configurable response cache for agents.
//
// The cache maps a normalized user question to a previously produced agent
// response so that asking the exact same (according to the configured
// normalization rules) question again returns the same answer without
// invoking the model.
//
// Two storage backends are supported:
//
//   - an in-memory map (the default), which keeps entries for the lifetime of
//     the process;
//   - a JSON-file backed store, which persists entries to disk so they
//     survive restarts.
//
// File-backed caches are concurrent-write-safe across processes. Every
// [Cache.Store] takes an exclusive advisory lock on a sibling
// `<path>.lock` file (POSIX flock / Windows LockFileEx), reloads the
// current on-disk state under the lock, merges its entry, and writes
// back atomically via a temp file + rename. Two processes simultaneously
// caching different keys both see their writes preserved; the lock
// serializes the read-modify-write window so neither can clobber the
// other. [Cache.Lookup] reloads the in-memory map when the file's mtime
// has advanced since its last load, so cross-process writes become
// visible without a restart.
//
// Two normalization options are exposed:
//
//   - case sensitivity: when disabled (the default), questions are
//     compared case-insensitively;
//   - blank trimming: when enabled, leading and trailing whitespace is
//     stripped before comparison.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config describes how a Cache should normalize keys and where it should
// store entries.
type Config struct {
	// Enabled toggles the cache on or off. When false, New returns nil.
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// CaseSensitive controls whether the question matching is
	// case-sensitive. The default (false) means "Hello" and "hello"
	// are treated as the same question.
	CaseSensitive bool `json:"case_sensitive,omitempty" yaml:"case_sensitive,omitempty"`

	// TrimSpaces controls whether leading and trailing whitespace is
	// removed from questions before they are compared. The default
	// (false) preserves whitespace.
	TrimSpaces bool `json:"trim_spaces,omitempty" yaml:"trim_spaces,omitempty"`

	// Path, when non-empty, selects a JSON-file backed cache stored at
	// the given path. When empty, the cache lives only in memory.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// Cache stores agent responses keyed on the user's question. It is safe
// for concurrent use by multiple goroutines, and — when configured with
// a Path — by multiple processes (see the package doc).
type Cache struct {
	mu        sync.RWMutex
	entries   map[string]string
	normalize func(string) string

	// path is the JSON file backing this cache, empty for in-memory only.
	path string

	// mtime is the file mtime at our last load. Lookup compares it
	// against the current file mtime to decide whether to reload, so
	// writes from a sibling process become visible without a restart.
	mtime time.Time
}

// New builds a Cache from the given Config. It returns (nil, nil) when
// caching is disabled, allowing callers to short-circuit with a simple
// nil check.
func New(cfg Config) (*Cache, error) {
	if !cfg.Enabled {
		return nil, nil //nolint:nilnil // intentional: nil signals caching disabled
	}

	c := &Cache{
		entries:   make(map[string]string),
		normalize: keyNormalizer(cfg.CaseSensitive, cfg.TrimSpaces),
		path:      cfg.Path,
	}

	if cfg.Path != "" {
		if err := loadFromFile(cfg.Path, c.entries); err != nil {
			return nil, err
		}
		c.mtime = mtimeOf(cfg.Path)
	}

	return c, nil
}

// Lookup returns the stored response for the given question and a
// boolean indicating whether the question was found.
//
// When the cache is file-backed and the file has been modified since
// our last load (typically by another process), the in-memory state is
// reloaded before the lookup so cross-process writes are visible.
func (c *Cache) Lookup(question string) (string, bool) {
	c.maybeReload()
	key := c.normalize(question)
	c.mu.RLock()
	defer c.mu.RUnlock()
	resp, ok := c.entries[key]
	return resp, ok
}

// Store records the response for the given question, replacing any
// existing entry with the same normalized key. Storing the same
// (question, response) pair twice is a no-op and skips the file
// rewrite — useful when an agent's stop hook re-fires with the same
// content (e.g. a cache-replay turn).
//
// When the cache is file-backed, the operation is serialized across
// processes via an advisory lock on a sibling .lock file: we take the
// lock, reload the on-disk state, merge our entry, write atomically
// (temp file + fsync + rename), and release. This guarantees that two
// processes simultaneously storing different keys both see their
// writes preserved on disk; in-process callers serialize via c.mu.
func (c *Cache) Store(question, response string) {
	key := c.normalize(question)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Cheap in-memory dedup: skip the cross-process lock and disk
	// write when our local view already matches. After a recent
	// Lookup our view is fresh, so this catches the common
	// "store the answer we just replayed" path of cache_response.
	if existing, ok := c.entries[key]; ok && existing == response {
		return
	}

	if c.path != "" {
		if err := c.persistToDisk(key, response); err != nil {
			// Persistence failures are not fatal: keep the entry
			// in memory and let the next Store retry the file
			// write.
			slog.Warn("cache persist failed; keeping entry in memory only",
				"path", c.path, "error", err)
		}
	}

	// Update in-memory map after persist (not before) so that if persist
	// fails, we still have the entry in memory for this process. The next
	// Lookup will reload from disk if another process wrote successfully.

	c.entries[key] = response
}

// persistToDisk takes the cross-process lock on c.path's sibling .lock
// file, reloads the on-disk entries, merges (key, response), writes
// atomically, and refreshes c.mtime. The caller must hold c.mu.
//
// Skips the write — but still refreshes c.mtime — when the on-disk
// state already has key → response, which keeps cross-process replays
// free of redundant disk traffic.
func (c *Cache) persistToDisk(key, response string) error {
	unlock, err := lockFile(c.path)
	if err != nil {
		return err
	}
	defer unlock()

	entries := make(map[string]string)
	if err := loadFromFile(c.path, entries); err != nil {
		return err
	}

	if existing, ok := entries[key]; ok && existing == response {
		c.mtime = mtimeOf(c.path)
		return nil
	}

	entries[key] = response
	if err := writeJSON(c.path, entries); err != nil {
		return err
	}
	c.mtime = mtimeOf(c.path)
	return nil
}

// maybeReload reloads c.entries from disk when the file mtime has
// advanced since our last load. Called from Lookup; a no-op when the
// cache is in-memory only or when the file can't be stat'd (in-memory
// state is preserved). Re-stats the file under the write lock to avoid
// TOCTOU races.
func (c *Cache) maybeReload() {
	if c.path == "" {
		return
	}
	info, err := os.Stat(c.path)
	if err != nil {
		return
	}

	c.mu.RLock()
	upToDate := info.ModTime().Equal(c.mtime)
	c.mu.RUnlock()
	if upToDate {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-stat under the write lock to avoid TOCTOU: the file could have
	// been modified between the initial stat and the lock acquisition.
	// Using the stale info would risk storing a mtime that doesn't match
	// the content we're about to load.
	info, err = os.Stat(c.path)
	if err != nil || info.ModTime().Equal(c.mtime) {
		// File disappeared or another goroutine already reloaded to this
		// mtime; nothing to do.
		return
	}
	fresh := make(map[string]string)
	if err := loadFromFile(c.path, fresh); err != nil {
		slog.Warn("cache reload failed; keeping in-memory state",
			"path", c.path, "error", err)
		return
	}
	c.entries = fresh
	c.mtime = info.ModTime()
}

// keyNormalizer returns a function that applies the configured
// normalization rules to a question before it is used as a cache key.
// Trim runs before lowercase: trim only removes leading/trailing
// whitespace and is unaffected by case, so the order is irrelevant for
// correctness, but trim-first keeps the lowercased map keys tighter on
// inputs like "  HELLO\n".
func keyNormalizer(caseSensitive, trimSpaces bool) func(string) string {
	return func(s string) string {
		if trimSpaces {
			s = strings.TrimSpace(s)
		}
		if !caseSensitive {
			s = strings.ToLower(s)
		}
		return s
	}
}

// loadFromFile decodes path into entries. A missing file is not an error
// and leaves entries empty.
func loadFromFile(path string, entries map[string]string) error {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("reading cache file %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("loading cache file %q: %w", path, err)
	}
	return nil
}

// mtimeOf returns the file modification time at path, or the zero value
// if path doesn't exist or can't be stat'd.
//
// The zero value (time.Time{}) is a sentinel meaning "file not found"
// and is safe to compare via [time.Time.Equal] in [Cache.maybeReload]: it
// will never equal a real mtime, so the reload will proceed as expected.
func mtimeOf(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// lockFile takes an exclusive advisory lock on "<path>.lock", creating
// the lock file (and any missing parent directory) if needed. The
// returned closure releases the lock and closes the descriptor; defer
// it in the caller.
//
// The lock file is intentionally never renamed or deleted: doing so
// would let two processes lock different inodes for the same logical
// resource and lose mutual exclusion. It's a long-lived sentinel.
func lockFile(path string) (func(), error) {
	lockPath := path + ".lock"
	if dir := filepath.Dir(lockPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating lock directory %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %q: %w", lockPath, err)
	}
	if err := lockExclusive(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %q: %w", lockPath, err)
	}
	// Errors in the release path are ignored: the OS will release the
	// lock when the descriptor is closed regardless, and a failure to
	// close at this point can't usefully be propagated.
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// writeJSON atomically writes entries to path as pretty-printed JSON.
//
// The new content is written to a sibling temp file, fsync'd, and renamed
// over the destination. POSIX guarantees the rename is atomic, so a
// concurrent reader sees either the previous content or the new content
// in full — never a partial write. The parent directory is fsync'd
// after the rename so the rename itself is durable across an OS crash.
func writeJSON(path string, entries map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating cache directory %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cache: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".cache-*.json")
	if err != nil {
		return fmt.Errorf("creating temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	// Cleanup on any error path; harmless once Rename has moved the file.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp cache file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp cache file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming cache file: %w", err)
	}

	syncDir(dir)
	return nil
}

// syncDir best-effort fsyncs a directory so a recent rename inside it is
// persisted. Directory fsync is not portable (e.g. unsupported on
// Windows); a failure here does not invalidate the data.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
