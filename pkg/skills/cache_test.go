package skills

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiskCache_FetchAndStore(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		fmt.Fprint(w, "file content")
	}))
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	content, err := cache.FetchAndStore(t.Context(), "https://example.com", "my-skill", "SKILL.md", srv.URL+"/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "file content", content)

	// Verify it was written to disk
	filePath := filepath.Join(cache.cacheDir("https://example.com", "my-skill"), "SKILL.md")
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "file content", string(data))

	// Verify metadata was written
	metaPath := filePath + ".meta"
	_, err = os.Stat(metaPath)
	require.NoError(t, err)
}

func TestDiskCache_Get_NotCached(t *testing.T) {
	t.Parallel()
	cache := newDiskCache(t.TempDir())

	_, ok := cache.Get("https://example.com", "nonexistent", "SKILL.md")
	assert.False(t, ok)
}

func TestDiskCache_Get_Cached(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		fmt.Fprint(w, "cached content")
	}))
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	_, err := cache.FetchAndStore(t.Context(), "https://example.com", "skill", "SKILL.md", srv.URL+"/SKILL.md")
	require.NoError(t, err)

	content, ok := cache.Get("https://example.com", "skill", "SKILL.md")
	assert.True(t, ok)
	assert.Equal(t, "cached content", content)
}

func TestDiskCache_Get_Expired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=0")
		fmt.Fprint(w, "expired content")
	}))
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	_, err := cache.FetchAndStore(t.Context(), "https://example.com", "skill", "SKILL.md", srv.URL+"/SKILL.md")
	require.NoError(t, err)

	// The max-age=0 should make it immediately expired
	_, ok := cache.Get("https://example.com", "skill", "SKILL.md")
	assert.False(t, ok)
}

func TestDiskCache_NestedFiles(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "nested file content")
	}))
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	content, err := cache.FetchAndStore(t.Context(), "https://example.com", "my-skill", "references/FORMS.md", srv.URL+"/file")
	require.NoError(t, err)
	assert.Equal(t, "nested file content", content)

	// Verify the nested directory was created
	filePath := filepath.Join(cache.cacheDir("https://example.com", "my-skill"), "references", "FORMS.md")
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "nested file content", string(data))
}

func TestDiskCache_DifferentURLsGetDifferentDirs(t *testing.T) {
	t.Parallel()
	cache := newDiskCache(t.TempDir())

	dir1 := cache.cacheDir("https://example.com", "skill")
	dir2 := cache.cacheDir("https://other.com", "skill")

	assert.NotEqual(t, dir1, dir2)
}

func TestParseCacheControl(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("empty header uses default", func(t *testing.T) {
		d := parseCacheControl("")
		assert.False(t, d.noStore)
		assert.False(t, d.noCache)
		assert.WithinDuration(t, now.Add(1*time.Hour), d.expiresAt(), 2*time.Second)
	})

	t.Run("max-age=3600", func(t *testing.T) {
		d := parseCacheControl("max-age=3600")
		assert.True(t, d.hasMaxAge)
		assert.WithinDuration(t, now.Add(3600*time.Second), d.expiresAt(), 2*time.Second)
	})

	t.Run("max-age=0", func(t *testing.T) {
		d := parseCacheControl("max-age=0")
		assert.True(t, d.hasMaxAge)
		assert.WithinDuration(t, now, d.expiresAt(), 2*time.Second)
	})

	t.Run("no-store forces immediate expiry", func(t *testing.T) {
		d := parseCacheControl("no-store")
		assert.True(t, d.noStore)
		assert.WithinDuration(t, now, d.expiresAt(), 2*time.Second)
	})

	t.Run("no-cache forces immediate expiry", func(t *testing.T) {
		d := parseCacheControl("no-cache")
		assert.True(t, d.noCache)
		assert.WithinDuration(t, now, d.expiresAt(), 2*time.Second)
	})

	t.Run("no-cache wins over max-age", func(t *testing.T) {
		d := parseCacheControl("max-age=3600, no-cache")
		assert.True(t, d.noCache)
		assert.WithinDuration(t, now, d.expiresAt(), 2*time.Second)
	})

	t.Run("multiple directives with max-age", func(t *testing.T) {
		d := parseCacheControl("public, max-age=7200")
		assert.WithinDuration(t, now.Add(7200*time.Second), d.expiresAt(), 2*time.Second)
	})

	t.Run("unknown directives use default", func(t *testing.T) {
		d := parseCacheControl("public")
		assert.WithinDuration(t, now.Add(1*time.Hour), d.expiresAt(), 2*time.Second)
	})
}

func TestDiskCache_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	_, err := cache.FetchAndStore(t.Context(), "https://example.com", "skill", "SKILL.md", srv.URL+"/notfound")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}

// TestDiskCache_NoStoreStoresButExpiresImmediately verifies that a
// Cache-Control: no-store response is still written to disk (so the
// in-process reader at pkg/tools/builtin/skills can consume it via
// readFileContent(skill.FilePath)) but is marked expired so the next
// Load() refetches instead of reusing the stored copy.
//
// We deliberately diverge from RFC 9111 §5.2.2.5 ("the cache MUST NOT
// store any part of either the immediate request or response") because
// the consumer reads files directly, not through diskCache.Get. Skipping
// the write entirely would render no-store skills unreadable for the
// rest of the process. A future refactor (in-memory cache shared with
// the reader) can make this strictly RFC-compliant.
func TestDiskCache_NoStoreStoresButExpiresImmediately(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, "private content")
	}))
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	content, err := cache.FetchAndStore(t.Context(), "https://example.com", "skill", "SKILL.md", srv.URL+"/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "private content", content)

	// The reader reads skill.FilePath directly, so the file must exist.
	filePath := filepath.Join(cache.cacheDir("https://example.com", "skill"), "SKILL.md")
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "private content", string(data))

	// But Get() must report a miss so prefetchFiles will refetch on the
	// next Load() cycle rather than reusing a stale entry.
	_, ok := cache.Get("https://example.com", "skill", "SKILL.md")
	assert.False(t, ok, "no-store must force a refetch on the next read")
}

// TestDiskCache_NoCacheStoresButExpiresImmediately verifies that no-cache
// allows storage but forces revalidation: the entry is written so it can be
// inspected, but Get() must report a miss so the next read refetches.
func TestDiskCache_NoCacheStoresButExpiresImmediately(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, "revalidate me")
	}))
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	_, err := cache.FetchAndStore(t.Context(), "https://example.com", "skill", "SKILL.md", srv.URL+"/SKILL.md")
	require.NoError(t, err)

	filePath := filepath.Join(cache.cacheDir("https://example.com", "skill"), "SKILL.md")
	_, err = os.Stat(filePath)
	require.NoError(t, err, "no-cache response should still be stored on disk")

	_, ok := cache.Get("https://example.com", "skill", "SKILL.md")
	assert.False(t, ok, "no-cache must force a refetch on the next read")
}

func TestDiskCache_FileSizeLimit(t *testing.T) {
	t.Parallel()
	for _, chunked := range []bool{false, true} {
		for _, size := range []int{maxCachedFileSize - 1, maxCachedFileSize, maxCachedFileSize + 1} {
			t.Run(fmt.Sprintf("chunked=%t/size=%d", chunked, size), func(t *testing.T) {
				t.Parallel()
				body := strings.Repeat("x", size)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if chunked {
						w.(http.Flusher).Flush()
					} else {
						w.Header().Set("Content-Length", strconv.Itoa(size))
					}
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				cache := newDiskCache(t.TempDir())
				content, err := cache.FetchAndStore(t.Context(), srv.URL, "skill", "SKILL.md", srv.URL)
				path := filepath.Join(cache.cacheDir(srv.URL, "skill"), "SKILL.md")
				if size > maxCachedFileSize {
					require.ErrorContains(t, err, "file exceeds")
					assert.Empty(t, content)
					assert.NoFileExists(t, path)
					assert.NoFileExists(t, path+".meta")
					_, ok := cache.Get(srv.URL, "skill", "SKILL.md")
					assert.False(t, ok)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, body, content)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, body, string(data))
				cached, ok := cache.Get(srv.URL, "skill", "SKILL.md")
				assert.True(t, ok)
				assert.Equal(t, body, cached)
			})
		}
	}
}

func TestDiskCache_OversizedResponsePreservesExistingEntry(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			_, _ = io.WriteString(w, strings.Repeat("x", maxCachedFileSize+1))
			return
		}
		_, _ = io.WriteString(w, "complete original")
	}))
	defer srv.Close()
	cache := newDiskCache(t.TempDir())
	_, err := cache.FetchAndStore(t.Context(), srv.URL, "skill", "SKILL.md", srv.URL)
	require.NoError(t, err)
	path := filepath.Join(cache.cacheDir(srv.URL, "skill"), "SKILL.md")
	meta, err := os.ReadFile(path + ".meta")
	require.NoError(t, err)

	_, err = cache.FetchAndStore(t.Context(), srv.URL, "skill", "SKILL.md", srv.URL+"/large")
	require.ErrorContains(t, err, "file exceeds")
	content, ok := cache.Get(srv.URL, "skill", "SKILL.md")
	assert.True(t, ok)
	assert.Equal(t, "complete original", content)
	after, err := os.ReadFile(path + ".meta")
	require.NoError(t, err)
	assert.Equal(t, meta, after)
}
