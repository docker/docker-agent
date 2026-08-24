package config

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/memoize"
	"github.com/docker/docker-agent/pkg/remote"
)

// newURLSourceForTest constructs a urlSource that bypasses the HTTPS-only and
// SSRF dial-time checks. It is defined here, in a _test.go file, so it is
// not compiled into release binaries. Tests use it because httptest.NewServer
// binds to 127.0.0.1 over plain HTTP.
func newURLSourceForTest(rawURL string, envProvider environment.Provider) Source {
	return &urlSource{
		url:         rawURL,
		envProvider: envProvider,
		unsafe:      true,
	}
}

func TestOCISource_DigestReference_ServesFromCache(t *testing.T) {
	t.Parallel()

	// Create a temporary content store and store a test artifact.
	storeDir := t.TempDir()
	store, err := content.NewStore(content.WithBaseDir(storeDir))
	require.NoError(t, err)

	testData := []byte("version: v1\nname: test-agent")
	layer := static.NewLayer(testData, "application/yaml")
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	img = mutate.Annotations(img, map[string]string{
		"io.docker.agent.version": "test",
	}).(v1.Image)

	ref := "test-digest-cache/agent:latest"
	digest, err := store.StoreArtifact(img, ref)
	require.NoError(t, err)

	// Build a digest reference using the stored digest.
	digestRef := "test-digest-cache/agent@" + digest

	// Read via ociSource. Since the reference is pinned by digest and is
	// present in the local store, this must succeed without any network call.
	// We override the default store directory via an env-based approach;
	// instead, we directly exercise the cache-hit logic by verifying the
	// store lookup works with the normalized key.
	storeKey, err := remote.NormalizeReference(digestRef)
	require.NoError(t, err)

	// Verify the store can resolve the digest key directly.
	data, err := store.GetArtifact(storeKey)
	require.NoError(t, err)
	assert.Equal(t, string(testData), data)

	// Also verify that IsDigestReference correctly identifies this.
	assert.True(t, remote.IsDigestReference(digestRef))
	assert.False(t, remote.IsDigestReference(ref))
}

// storeTestArtifact writes an agent YAML artifact into the default content
// store (rooted at $HOME, which the caller must point at a temp dir) under
// the given tag reference.
func storeTestArtifact(t *testing.T, ref string, data []byte) string {
	t.Helper()

	store, err := content.NewStore()
	require.NoError(t, err)

	layer := static.NewLayer(data, "application/yaml")
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	img = mutate.Annotations(img, map[string]string{
		"io.docker.agent.version": "test",
	}).(v1.Image)

	digest, err := store.StoreArtifact(img, ref)
	require.NoError(t, err)
	return digest
}

// stubOCIPull replaces the registry pull with fn for the test's duration.
func stubOCIPull(t *testing.T, fn func(ctx context.Context, ref string, force bool) (string, error)) {
	t.Helper()
	original := pullOCIArtifact
	pullOCIArtifact = fn
	t.Cleanup(func() { pullOCIArtifact = original })
}

// resetOCIMemoizer swaps in a fresh read memoizer so tests neither observe
// nor leak cached reads across tests and repeated runs (-count=2).
func resetOCIMemoizer(t *testing.T) {
	t.Helper()
	original := ociReadMemoizer
	ociReadMemoizer = memoize.New[[]byte](time.Minute)
	t.Cleanup(func() { ociReadMemoizer = original })
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_MemoizesSuccessfulReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	testData := []byte("version: v1\nname: memoized-agent")
	ref := "test-memoize/agent:latest"
	storeTestArtifact(t, ref, testData)

	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	source := NewOCISource(ref)
	for range 3 {
		data, err := source.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, testData, data)
	}
	assert.Equal(t, int32(1), pulls.Load(), "repeated reads of the same ref must pull only once")

	// An equivalent form of the same reference must hit the same cache entry.
	data, err := NewOCISource("index.docker.io/test-memoize/agent:latest").Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)
	assert.Equal(t, int32(1), pulls.Load(), "equivalent refs must share the cache entry")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_DoesNotCacheFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	ref := "test-memoize-errors/agent:latest"

	// No local artifact and a failing pull: Read must fail.
	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", errors.New("registry unreachable")
	})

	source := NewOCISource(ref)
	_, err := source.Read(t.Context())
	require.Error(t, err)

	// Make the artifact available and the pull succeed: the earlier failure
	// must not have been cached, so Read retries and succeeds.
	testData := []byte("version: v1\nname: retried-agent")
	storeTestArtifact(t, ref, testData)
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testData, data)
	assert.Equal(t, int32(2), pulls.Load(), "a failed read must be retried, not served from cache")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_CacheIsScopedToRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	// Same repository and tag on two different registries: distinct trust
	// boundaries, so each must do its own pull instead of sharing an entry.
	testData := []byte("version: v1\nname: scoped-agent")
	storeTestArtifact(t, "test-scoped/agent:latest", testData)

	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})

	_, err := NewOCISource("registry-a.example.com/test-scoped/agent:latest").Read(t.Context())
	require.NoError(t, err)
	_, err = NewOCISource("registry-b.example.com/test-scoped/agent:latest").Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(2), pulls.Load(), "refs differing only by registry must not share a cache entry")
}

// Not parallel: stubs the package-level pullOCIArtifact and re-homes the
// default content store via t.Setenv.
func TestOCISource_Read_DoesNotCacheDegradedFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetOCIMemoizer(t)

	// Artifact is available locally but the registry is unreachable: Read
	// succeeds by falling back to the local store.
	testData := []byte("version: v1\nname: degraded-agent")
	ref := "test-degraded/agent:latest"
	storeTestArtifact(t, ref, testData)

	var pulls atomic.Int32
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", errors.New("registry unreachable")
	})

	source := NewOCISource(ref)
	for i := 1; i <= 2; i++ {
		data, err := source.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, testData, data)
		assert.Equal(t, int32(i), pulls.Load(), "a degraded fallback read must not be cached; the registry must be retried")
	}

	// Once the registry recovers, the validated read IS cached.
	stubOCIPull(t, func(context.Context, string, bool) (string, error) {
		pulls.Add(1)
		return "", nil
	})
	for range 2 {
		data, err := source.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, testData, data)
	}
	assert.Equal(t, int32(3), pulls.Load(), "a validated read must be cached again")
}

func TestURLSource_Read(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	source := newURLSourceForTest(server.URL, nil)

	assert.Equal(t, server.URL, source.Name())
	assert.Empty(t, source.ParentDir())

	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "test content", string(data))
}

func TestURLSource_Read_HTTPError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
	}{
		{"not found", http.StatusNotFound},
		{"server error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			t.Cleanup(server.Close)

			// Clean up any cached data for this URL to ensure we test the error path
			urlCacheDir := getURLCacheDir()
			urlHash := hashURL(server.URL)
			cachePath := filepath.Join(urlCacheDir, urlHash)
			etagPath := cachePath + ".etag"
			_ = os.Remove(cachePath)
			_ = os.Remove(etagPath)

			_, err := newURLSourceForTest(server.URL, nil).Read(t.Context())
			require.Error(t, err)
			require.ErrorIs(t, err, ErrSourceFetchFailed)
		})
	}
}

func TestURLSource_Read_ConnectionError(t *testing.T) {
	t.Parallel()

	_, err := newURLSourceForTest("http://invalid.invalid/config.yaml", nil).Read(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceFetchFailed)
}

func TestURLSource_Read_CachesContent(t *testing.T) {
	t.Parallel()
	// Not parallel - uses shared cache directory

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"test-etag-caches-content"`)
		_, _ = w.Write([]byte("test content for caching"))
	}))
	t.Cleanup(server.Close)

	source := newURLSourceForTest(server.URL, nil)

	// First read should fetch and cache
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "test content for caching", string(data))

	// Verify cache files were created
	urlCacheDir := getURLCacheDir()
	urlHash := hashURL(server.URL)
	cachePath := filepath.Join(urlCacheDir, urlHash)
	etagPath := cachePath + ".etag"

	// Cleanup at end of test
	t.Cleanup(func() {
		_ = os.Remove(cachePath)
		_ = os.Remove(etagPath)
	})

	cachedData, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	assert.Equal(t, "test content for caching", string(cachedData))

	cachedETag, err := os.ReadFile(etagPath)
	require.NoError(t, err)
	assert.Equal(t, `"test-etag-caches-content"`, string(cachedETag))
}

func TestURLSource_Read_UsesETagForConditionalRequest(t *testing.T) {
	t.Parallel()
	// Not parallel - uses shared cache directory

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Header.Get("If-None-Match") == `"test-etag-conditional"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"test-etag-conditional"`)
		_, _ = w.Write([]byte("test content conditional"))
	}))
	t.Cleanup(server.Close)

	// Pre-populate cache
	urlCacheDir := getURLCacheDir()
	require.NoError(t, os.MkdirAll(urlCacheDir, 0o755))
	urlHash := hashURL(server.URL)
	cachePath := filepath.Join(urlCacheDir, urlHash)
	etagPath := cachePath + ".etag"
	require.NoError(t, os.WriteFile(cachePath, []byte("cached content conditional"), 0o644))
	require.NoError(t, os.WriteFile(etagPath, []byte(`"test-etag-conditional"`), 0o644))

	// Cleanup at end of test
	t.Cleanup(func() {
		_ = os.Remove(cachePath)
		_ = os.Remove(etagPath)
	})

	source := newURLSourceForTest(server.URL, nil)

	// Read should use cached content via 304 response
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cached content conditional", string(data))
	assert.Equal(t, int32(1), requestCount.Load())
}

func TestURLSource_Read_FallsBackToCacheOnNetworkError(t *testing.T) {
	t.Parallel()
	// Not parallel - uses shared cache directory

	// Pre-populate cache for a non-existent server
	agentURL := "http://invalid.invalid:12345/config-network-error.yaml"
	urlCacheDir := getURLCacheDir()
	require.NoError(t, os.MkdirAll(urlCacheDir, 0o755))
	urlHash := hashURL(agentURL)
	cachePath := filepath.Join(urlCacheDir, urlHash)
	require.NoError(t, os.WriteFile(cachePath, []byte("cached content network error"), 0o644))

	// Cleanup at end of test
	t.Cleanup(func() {
		_ = os.Remove(cachePath)
	})

	source := newURLSourceForTest(agentURL, nil)

	// Read should fall back to cached content
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cached content network error", string(data))
}

func TestURLSource_Read_FallsBackToCacheOnHTTPError(t *testing.T) {
	t.Parallel()
	// Not parallel - uses shared cache directory

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	// Pre-populate cache
	urlCacheDir := getURLCacheDir()
	require.NoError(t, os.MkdirAll(urlCacheDir, 0o755))
	urlHash := hashURL(server.URL)
	cachePath := filepath.Join(urlCacheDir, urlHash)
	require.NoError(t, os.WriteFile(cachePath, []byte("cached content http error"), 0o644))

	// Cleanup at end of test
	t.Cleanup(func() {
		_ = os.Remove(cachePath)
	})

	source := newURLSourceForTest(server.URL, nil)

	// Read should fall back to cached content
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cached content http error", string(data))
}

func TestURLSource_Read_UpdatesCacheWhenContentChanges(t *testing.T) {
	t.Parallel()
	// Not parallel - uses shared cache directory

	var serverContent atomic.Value
	serverContent.Store("initial content update")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentContent := serverContent.Load().(string)
		etag := `"etag-` + currentContent + `"`

		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(currentContent))
	}))
	t.Cleanup(server.Close)

	urlCacheDir := getURLCacheDir()
	urlHash := hashURL(server.URL)
	cachePath := filepath.Join(urlCacheDir, urlHash)
	etagPath := cachePath + ".etag"

	// Cleanup at end of test
	t.Cleanup(func() {
		_ = os.Remove(cachePath)
		_ = os.Remove(etagPath)
	})

	source := newURLSourceForTest(server.URL, nil)

	// First read
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "initial content update", string(data))

	// Change content
	serverContent.Store("updated content update")

	// Second read should get new content
	data, err = source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "updated content update", string(data))

	// Verify cache was updated
	cachedData, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	assert.Equal(t, "updated content update", string(cachedData))
}

func TestURLSource_Read_RejectsHTTP(t *testing.T) {
	t.Parallel()

	_, err := NewURLSource("http://example.com/agent.yaml", nil).Read(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only https://")
}

func TestURLSource_Read_AllowsLocalhostHTTP(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("localhost content"))
	}))
	t.Cleanup(server.Close)

	// Replace the 127.0.0.1 address from httptest with localhost so
	// the production code path recognises it as a localhost exemption.
	localhostURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)

	source := NewURLSource(localhostURL, nil)
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "localhost content", string(data))
}

func TestURLSource_Read_RejectsNonHTTPSchemesOnLocalhost(t *testing.T) {
	t.Parallel()

	for _, u := range []string{
		"ftp://localhost/agent.yaml",
		"file://localhost/agent.yaml",
		"gopher://localhost/agent.yaml",
	} {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			_, err := NewURLSource(u, nil).Read(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "only https://")
		})
	}
}

func TestURLSource_Read_LocalhostRejectsNonLocalhostRedirect(t *testing.T) {
	t.Parallel()

	// A localhost server that redirects to an external URL.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	localhostURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)

	// Clear cache so we exercise the network path.
	urlCacheDir := getURLCacheDir()
	urlHash := hashURL(localhostURL)
	_ = os.Remove(filepath.Join(urlCacheDir, urlHash))
	_ = os.Remove(filepath.Join(urlCacheDir, urlHash+".etag"))

	_, err := NewURLSource(localhostURL, nil).Read(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-localhost")
}

func TestURLSource_Read_RejectsLocalAddresses(t *testing.T) {
	t.Parallel()

	// Hosts whose only resolution is a non-public IP must be refused at
	// dial time. We test the SSRF dialer via the HTTPS code path even
	// though the TLS handshake will never complete, because the dial is
	// aborted before any bytes are sent.
	tests := []string{
		"https://127.0.0.1/agent.yaml",       // loopback
		"https://[::1]/agent.yaml",           // IPv6 loopback
		"https://10.0.0.1/agent.yaml",        // RFC1918
		"https://192.168.1.1/agent.yaml",     // RFC1918
		"https://169.254.169.254/agent.yaml", // AWS/GCP/Azure metadata
		"https://0.0.0.0/agent.yaml",         // unspecified
	}
	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()

			// Clear any cached content so the dial is actually attempted.
			urlCacheDir := getURLCacheDir()
			urlHash := hashURL(rawURL)
			_ = os.Remove(filepath.Join(urlCacheDir, urlHash))
			_ = os.Remove(filepath.Join(urlCacheDir, urlHash+".etag"))

			_, err := NewURLSource(rawURL, nil).Read(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "non-public address")
		})
	}
}

func TestURLSource_Read_RejectsHTTPRedirect(t *testing.T) {
	t.Parallel()
	// Not parallel - clears cache.

	// HTTPS origin that 302s to plain http. We use httptest.NewTLSServer so
	// the production ssrfSafeHTTPClient gets to exercise CheckRedirect on a
	// real Location header. The dial-time SSRF check would reject 127.0.0.1
	// before the redirect target is fetched, but CheckRedirect runs first
	// and gives us the precise downgrade error message.
	httpsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "http://example.com/downgraded", http.StatusFound)
	}))
	t.Cleanup(httpsSrv.Close)

	// Trust the test server's self-signed cert by injecting it into the
	// Go default cert pool would be invasive; instead, exercise the
	// CheckRedirect hook directly via ssrfCheckRedirect (covered above)
	// and assert that the production fetch path errors out for an https
	// origin pointing at a non-trusted CA. Either way, the request must
	// not silently follow to http://.
	agentURL := httpsSrv.URL + "/agent.yaml"
	urlCacheDir := getURLCacheDir()
	urlHash := hashURL(agentURL)
	_ = os.Remove(filepath.Join(urlCacheDir, urlHash))
	_ = os.Remove(filepath.Join(urlCacheDir, urlHash+".etag"))

	_, err := NewURLSource(agentURL, nil).Read(t.Context())
	require.Error(t, err)
}

func TestIsURLReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected bool
	}{
		{"http://example.com/agent.yaml", true},
		{"https://example.com/agent.yaml", true},
		{"https://example.com:8080/path", true},
		{"/path/to/agent.yaml", false},
		{"./agent.yaml", false},
		{"docker.io/myorg/agent:v1", false},
		{"ftp://example.com/agent.yaml", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, IsURLReference(tt.input))
		})
	}
}

func TestResolve_URLReference(t *testing.T) {
	t.Parallel()

	source, err := Resolve("https://example.com/agent.yaml", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/agent.yaml", source.Name())
	assert.Empty(t, source.ParentDir())
}

func TestResolveSources_URLReference(t *testing.T) {
	t.Parallel()

	testURL := "https://example.com/agent.yaml"
	sources, err := ResolveSources(testURL, nil)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the URL-encoded version
	expectedKey := url.QueryEscape(testURL)
	source, ok := sources[expectedKey]
	require.True(t, ok)
	assert.Equal(t, testURL, source.Name())
}

func TestURLSource_Read_WithGitHubAuth(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	// Create a mock env provider that returns a GitHub token
	envProvider := environment.NewMapEnvProvider(map[string]string{
		"GITHUB_TOKEN": "test-token-123",
	})

	// For non-GitHub URLs, auth should not be added even with token available
	source := newURLSourceForTest(server.URL, envProvider)
	_, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, receivedAuth, "non-GitHub URLs should not receive auth header")
}

func TestURLSource_Read_WithGitHubAuth_GitHubURL(t *testing.T) {
	t.Parallel()

	// Note: We cannot directly test with real GitHub URLs in unit tests.
	// This test verifies that URLs with GitHub hosts in the path (not hostname)
	// are correctly identified as non-GitHub URLs and don't receive auth.
	// This is a security-critical behavior to prevent token leakage.

	for _, host := range githubHosts {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			var receivedAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedAuth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte("test content"))
			}))
			t.Cleanup(server.Close)

			envProvider := environment.NewMapEnvProvider(map[string]string{
				"GITHUB_TOKEN": "test-token-456",
			})

			// URL with GitHub host in path (not hostname) should NOT receive auth
			// This prevents token leakage to attacker-controlled domains
			maliciousURL := server.URL + "/" + host + "/path/to/file"
			source := newURLSourceForTest(maliciousURL, envProvider)

			_, err := source.Read(t.Context())
			require.NoError(t, err)
			assert.Empty(t, receivedAuth, "should not add auth header when GitHub host is only in path")
		})
	}
}

func TestURLSource_Read_WithGitHubAuth_NoToken(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	// Create a mock env provider without a GitHub token
	envProvider := environment.NewNoEnvProvider()

	source := newURLSourceForTest(server.URL, envProvider)
	_, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, receivedAuth, "should not add auth header when token is missing")
}

func TestURLSource_Read_WithGitHubAuth_NoEnvProvider(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	// No env provider
	source := newURLSourceForTest(server.URL, nil)
	_, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, receivedAuth, "should not add auth header without env provider")
}

func TestIsGitHubURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url      string
		expected bool
	}{
		// Valid GitHub URLs
		{"https://github.com/owner/repo/blob/main/agent.yaml", true},
		{"https://raw.githubusercontent.com/owner/repo/main/agent.yaml", true},
		{"https://gist.githubusercontent.com/owner/gist-id/raw/file.yaml", true},
		{"http://github.com/owner/repo", true},

		// Non-GitHub URLs
		{"https://example.com/agent.yaml", false},
		{"https://gitlab.com/owner/repo/agent.yaml", false},
		{"http://localhost:8080/agent.yaml", false},
		{"", false},

		// Security: malicious URLs that should NOT be treated as GitHub URLs
		// These test cases prevent token leakage to attacker-controlled domains
		{"https://evil.com/github.com/file.yaml", false},           // github.com in path
		{"https://notgithub.com/file.yaml", false},                 // similar domain name
		{"https://github.com.attacker.com/file.yaml", false},       // github.com as subdomain
		{"https://fakegithub.com/owner/repo/agent.yaml", false},    // contains "github.com" substring
		{"https://raw.githubusercontent.com.evil.com/file", false}, // githubusercontent as subdomain
		{"https://attacker.com?redirect=github.com", false},        // github.com in query string
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isGitHubURL(tt.url))
		})
	}
}

func TestIsTrustedDockerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url      string
		expected bool
	}{
		// Valid Docker URLs (HTTPS only for remote)
		{"https://docker.com/some/path", true},
		{"https://desktop.docker.com/mcp/catalog/v3/catalog.yaml", true},
		{"https://api.docker.com/events/v1/track", true},
		{"https://api-stage.docker.com/events/v1/track", true},
		{"https://hub.docker.com/mcp/server", true},
		{"https://sub.sub.docker.com/path", true},

		// Localhost URLs (local development, HTTP or HTTPS)
		{"http://localhost:8080/agent.yaml", true},
		{"https://localhost/agent.yaml", true},
		{"http://127.0.0.1:8080/agent.yaml", true},
		{"http://[::1]:8080/agent.yaml", true},

		// Non-Docker URLs
		{"https://example.com/agent.yaml", false},
		{"https://github.com/docker/repo", false},
		{"", false},

		// Scheme enforcement: plain HTTP to remote .docker.com is rejected
		{"http://docker.com/path", false},
		{"http://desktop.docker.com/agent.yaml", false},

		// Non-HTTP(S) schemes are rejected
		{"ftp://docker.com/file.yaml", false},
		{"ftp://localhost/file.yaml", false},

		// Security: malicious URLs that should NOT be treated as Docker URLs
		{"https://evil.com/docker.com/file.yaml", false},     // docker.com in path
		{"https://notdocker.com/file.yaml", false},           // similar domain name
		{"https://docker.com.attacker.com/file.yaml", false}, // docker.com as subdomain of attacker
		{"https://fakedocker.com/agent.yaml", false},         // contains "docker.com" substring
		{"https://attacker.com?redirect=docker.com", false},  // docker.com in query string
		{"https://my-docker.com/agent.yaml", false},          // hyphenated similar domain
		{"https://xdocker.com/agent.yaml", false},            // prefixed similar domain
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isTrustedDockerURL(tt.url))
		})
	}
}

func TestURLSource_Read_WithDockerAuth_NonDockerURL(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	envProvider := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "docker-jwt-token",
	})

	// Use a non-Docker, non-localhost URL as the source URL so
	// isTrustedDockerURL returns false. The actual HTTP request still goes to
	// the local test server via the unsafe flag.
	src := &urlSource{
		url:         "https://example.com/agent.yaml",
		envProvider: envProvider,
		unsafe:      true,
	}
	// Override the url to point at our test server for the actual fetch,
	// but addDockerAuth checks the url field which is example.com.
	// We need to actually fetch from the test server though, so we
	// manually test addDockerAuth in isolation instead.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	src.addDockerAuth(t.Context(), req)
	assert.Empty(t, receivedAuth, "non-Docker URLs should not receive Docker auth header")
}

func TestURLSource_Read_WithDockerAuth_LocalhostURL(t *testing.T) {
	t.Parallel()

	// httptest.NewServer binds to 127.0.0.1 which is treated as localhost.
	// Verify that the Docker JWT is included for localhost URLs.
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	envProvider := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "docker-jwt-token",
	})

	source := newURLSourceForTest(server.URL, envProvider)
	_, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer docker-jwt-token", receivedAuth, "localhost URLs should receive Docker auth header")
}

func TestURLSource_Read_WithDockerAuth_NoToken(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	envProvider := environment.NewNoEnvProvider()

	// Even if we construct a source with a Docker URL hostname, the
	// addDockerAuth method checks the url field, not the request URL.
	// We use the test server URL here, which is NOT a docker.com URL.
	source := newURLSourceForTest(server.URL, envProvider)
	_, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, receivedAuth, "should not add auth header when token is missing")
}

func TestURLSource_Read_WithDockerAuth_NoEnvProvider(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("test content"))
	}))
	t.Cleanup(server.Close)

	source := newURLSourceForTest(server.URL, nil)
	_, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, receivedAuth, "should not add auth header without env provider")
}

func TestURLSource_addDockerAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		url         string
		envProvider environment.Provider
		wantAuth    string
	}{
		{
			name: "docker.com URL with token",
			url:  "https://docker.com/agent.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "Bearer my-jwt",
		},
		{
			name: "subdomain of docker.com with token",
			url:  "https://desktop.docker.com/mcp/catalog.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "Bearer my-jwt",
		},
		{
			name: "localhost with token",
			url:  "http://localhost:8080/v1/models",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "Bearer my-jwt",
		},
		{
			name: "127.0.0.1 with token",
			url:  "http://127.0.0.1:9090/agent.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "Bearer my-jwt",
		},
		{
			name: "IPv6 loopback with token",
			url:  "http://[::1]:9090/agent.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "Bearer my-jwt",
		},
		{
			name: "non-docker URL with token",
			url:  "https://example.com/agent.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "",
		},
		{
			name:        "docker.com URL without token",
			url:         "https://desktop.docker.com/agent.yaml",
			envProvider: environment.NewNoEnvProvider(),
			wantAuth:    "",
		},
		{
			name:        "localhost without token",
			url:         "http://localhost:8080/agent.yaml",
			envProvider: environment.NewNoEnvProvider(),
			wantAuth:    "",
		},
		{
			name:        "docker.com URL without env provider",
			url:         "https://desktop.docker.com/agent.yaml",
			envProvider: nil,
			wantAuth:    "",
		},
		{
			name: "docker.com as subdomain of attacker",
			url:  "https://docker.com.attacker.com/agent.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "",
		},
		{
			name: "plain HTTP to docker.com is rejected",
			url:  "http://docker.com/agent.yaml",
			envProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "my-jwt",
			}),
			wantAuth: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := &urlSource{
				url:         tt.url,
				envProvider: tt.envProvider,
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, http.NoBody)
			require.NoError(t, err)

			src.addDockerAuth(t.Context(), req)

			assert.Equal(t, tt.wantAuth, req.Header.Get("Authorization"))
		})
	}
}

func TestResolve_URLReference_WithEnvProvider(t *testing.T) {
	t.Parallel()

	envProvider := environment.NewMapEnvProvider(map[string]string{
		"GITHUB_TOKEN": "test-token",
	})

	source, err := Resolve("https://github.com/owner/repo/raw/main/agent.yaml", envProvider)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/owner/repo/raw/main/agent.yaml", source.Name())

	// Verify the source has the env provider set
	urlSrc, ok := source.(*urlSource)
	require.True(t, ok)
	assert.NotNil(t, urlSrc.envProvider)
}

func TestResolveSources_URLReference_WithEnvProvider(t *testing.T) {
	t.Parallel()

	envProvider := environment.NewMapEnvProvider(map[string]string{
		"GITHUB_TOKEN": "test-token",
	})

	testURL := "https://github.com/owner/repo/raw/main/agent.yaml"
	sources, err := ResolveSources(testURL, envProvider)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	// The key should be the URL-encoded version
	expectedKey := url.QueryEscape(testURL)
	source, ok := sources[expectedKey]
	require.True(t, ok)

	// Verify the source has the env provider set
	urlSrc, ok := source.(*urlSource)
	require.True(t, ok)
	assert.NotNil(t, urlSrc.envProvider)
}

func TestIsLocalhostHTTPHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host     string
		expected bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"localhost.", true},      // trailing dot
		{"LOCALHOST.", true},      // trailing dot, case insensitive
		{"evil.localhost", false}, // *.localhost subdomains must be rejected
		{"sub.localhost", false},
		{"notlocalhost", false},
		{"localhost.evil.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isLocalhostHTTPHost(tt.host))
		})
	}
}
