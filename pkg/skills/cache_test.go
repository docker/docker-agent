package skills

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiskCache_FetchAndStore(t *testing.T) {
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
	cache := newDiskCache(t.TempDir())

	_, ok := cache.Get("https://example.com", "nonexistent", "SKILL.md")
	assert.False(t, ok)
}

func TestDiskCache_Get_Cached(t *testing.T) {
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
	cache := newDiskCache(t.TempDir())

	dir1 := cache.cacheDir("https://example.com", "skill")
	dir2 := cache.cacheDir("https://other.com", "skill")

	assert.NotEqual(t, dir1, dir2)
}

func TestParseCacheExpiry(t *testing.T) {
	now := time.Now()

	t.Run("empty header uses default", func(t *testing.T) {
		expiry := parseCacheExpiry("")
		assert.WithinDuration(t, now.Add(1*time.Hour), expiry, 2*time.Second)
	})

	t.Run("max-age=3600", func(t *testing.T) {
		expiry := parseCacheExpiry("max-age=3600")
		assert.WithinDuration(t, now.Add(3600*time.Second), expiry, 2*time.Second)
	})

	t.Run("max-age=0", func(t *testing.T) {
		expiry := parseCacheExpiry("max-age=0")
		assert.WithinDuration(t, now, expiry, 2*time.Second)
	})

	t.Run("no-store", func(t *testing.T) {
		expiry := parseCacheExpiry("no-store")
		assert.WithinDuration(t, now, expiry, 2*time.Second)
	})

	t.Run("no-cache", func(t *testing.T) {
		expiry := parseCacheExpiry("no-cache")
		assert.WithinDuration(t, now, expiry, 2*time.Second)
	})

	t.Run("multiple directives with max-age", func(t *testing.T) {
		expiry := parseCacheExpiry("public, max-age=7200")
		assert.WithinDuration(t, now.Add(7200*time.Second), expiry, 2*time.Second)
	})

	t.Run("unknown directives use default", func(t *testing.T) {
		expiry := parseCacheExpiry("public")
		assert.WithinDuration(t, now.Add(1*time.Hour), expiry, 2*time.Second)
	})
}

func TestDiskCache_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	cache := newDiskCache(t.TempDir())

	_, err := cache.FetchAndStore(t.Context(), "https://example.com", "skill", "SKILL.md", srv.URL+"/notfound")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}
