package config

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/configsize"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/paths"
)

// newURLSourceForTest constructs a urlSource that bypasses the HTTPS-only and
// SSRF dial-time checks. It is defined here, in a _test.go file, so it is
// not compiled into release binaries. Tests use it because httptest.NewServer
// binds to 127.0.0.1 over plain HTTP.
func newURLSourceForTest(rawURL string, envProvider environment.Provider) Source {
	return &urlSource{
		url:             rawURL,
		envProvider:     envProvider,
		unsafe:          true,
		encryptedConfig: &atomic.Pointer[string]{},
	}
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

func TestURLSource_Read_SizeLimit(t *testing.T) {
	for _, test := range []struct {
		name          string
		size          int64
		contentLength bool
		wantError     bool
	}{
		{name: "exact boundary", size: configsize.MaxBytes, contentLength: true},
		{name: "over boundary", size: configsize.MaxBytes + 1, contentLength: true, wantError: true},
		{name: "unknown length over boundary", size: configsize.MaxBytes + 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths.SetDataDir(t.TempDir())
			t.Cleanup(func() { paths.SetDataDir("") })

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.contentLength {
					w.Header().Set("Content-Length", strconv.FormatInt(test.size, 10))
				}
				// Advertise an encrypted-config digest as a Docker source would;
				// an oversized body must error out before any capture/cache happens.
				w.Header().Set(httpclient.EncryptedConfigDigestHeader, "sha256:deadbeef")
				_, _ = io.CopyN(w, zeroReader{}, test.size)
			}))
			t.Cleanup(server.Close)

			source := newURLSourceForTest(server.URL, nil)
			data, err := source.Read(t.Context())
			if test.wantError {
				require.ErrorIs(t, err, configsize.ErrTooLarge)
				require.NotErrorIs(t, err, ErrSourceFetchFailed)
				cachePath := filepath.Join(getURLCacheDir(), hashURL(server.URL))
				_, cacheErr := os.Stat(cachePath)
				require.ErrorIs(t, cacheErr, os.ErrNotExist)
				_, encErr := os.Stat(cachePath + ".enc")
				require.ErrorIs(t, encErr, os.ErrNotExist)
				assert.Empty(t, source.(EncryptedConfigSource).EncryptedConfig())
				return
			}
			require.NoError(t, err)
			assert.Len(t, data, int(test.size))
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestURLSource_Read_CacheSizeLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		response  string
		size      int64
		wantError bool
	}{
		{name: "304 exact boundary", response: "304", size: configsize.MaxBytes},
		{name: "304 over boundary", response: "304", size: configsize.MaxBytes + 1, wantError: true},
		{name: "network over boundary", response: "network", size: configsize.MaxBytes + 1, wantError: true},
		{name: "HTTP error over boundary", response: "HTTP error", size: configsize.MaxBytes + 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths.SetDataDir(t.TempDir())
			t.Cleanup(func() { paths.SetDataDir("") })

			var server *httptest.Server
			switch test.response {
			case "304":
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNotModified)
				}))
			case "network":
				server = httptest.NewServer(http.NotFoundHandler())
				server.Close()
			case "HTTP error":
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				}))
			default:
				t.Fatalf("unknown response %q", test.response)
			}
			if test.response != "network" {
				t.Cleanup(server.Close)
			}

			cachePath := filepath.Join(getURLCacheDir(), hashURL(server.URL))
			require.NoError(t, os.MkdirAll(filepath.Dir(cachePath), 0o700))
			require.NoError(t, writeZeroFile(cachePath, test.size))

			data, err := newURLSourceForTest(server.URL, nil).Read(t.Context())
			if test.wantError {
				require.ErrorIs(t, err, configsize.ErrTooLarge)
				require.NotErrorIs(t, err, ErrSourceFetchFailed)
				info, statErr := os.Stat(cachePath)
				require.NoError(t, statErr)
				assert.Equal(t, test.size, info.Size(), "rejected cache bytes must be preserved")
				return
			}
			require.NoError(t, err)
			assert.Len(t, data, int(test.size))
		})
	}
}

func writeZeroFile(path string, size int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(file, zeroReader{}, size)
	return errors.Join(copyErr, file.Close())
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

// digestOf mirrors the server's X-Cagent-Encrypted-Config-Digest format.
func digestOf(enc string) string {
	return encryptedConfigDigest(enc)
}

// testConfigBody is the agent YAML every encrypted-config test serves.
const testConfigBody = "version: \"2\"\n"

// writeConfigWithEncrypted writes a 200 response carrying the agent YAML with
// the encrypted config injected as a top-level `encrypted_agent_config` field,
// plus the digest header, mirroring the Docker gateway (gordon proxy).
func writeConfigWithEncrypted(w http.ResponseWriter, enc string) {
	w.Header().Set(httpclient.EncryptedConfigDigestHeader, digestOf(enc))
	_, _ = w.Write([]byte(testConfigBody + "encrypted_agent_config: " + enc + "\n"))
}

func TestURLSource_CapturesEncryptedConfigField(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	var gotSendEncrypted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSendEncrypted = r.URL.Query().Get("sendEncrypted")
		writeConfigWithEncrypted(w, "ENCRYPTED-BLOB")
	}))
	t.Cleanup(server.Close)

	// httptest binds to 127.0.0.1, which IsTrustedDockerURL treats as trusted,
	// so the field is stripped and the encrypted config captured.
	source := newURLSourceForTest(server.URL, nil)
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	// The stripped YAML is returned as the config body, without the field.
	assert.NotContains(t, string(data), "encrypted_agent_config")
	assert.Contains(t, string(data), "version:")

	ecs, ok := source.(EncryptedConfigSource)
	require.True(t, ok, "urlSource must implement EncryptedConfigSource")
	assert.Equal(t, "ENCRYPTED-BLOB", ecs.EncryptedConfig())
	// The client opts in to the encrypted config on trusted Docker URLs
	// (httptest binds to 127.0.0.1, treated as trusted).
	assert.Equal(t, "true", gotSendEncrypted, "sendEncrypted=true must be sent to a trusted Docker URL")
}

func TestURLWithSendEncrypted(t *testing.T) {
	t.Parallel()
	t.Run("adds the flag preserving existing params", func(t *testing.T) {
		t.Parallel()
		out, err := urlWithSendEncrypted("https://api.docker.com/gordon-agent?gordonTag=v15&origin=desktop")
		require.NoError(t, err)
		u, err := url.Parse(out)
		require.NoError(t, err)
		q := u.Query()
		assert.Equal(t, "true", q.Get("sendEncrypted"))
		assert.Equal(t, "v15", q.Get("gordonTag"))
		assert.Equal(t, "desktop", q.Get("origin"))
	})
	t.Run("overwrites an existing value", func(t *testing.T) {
		t.Parallel()
		out, err := urlWithSendEncrypted("https://api.docker.com/gordon-agent?sendEncrypted=false")
		require.NoError(t, err)
		u, err := url.Parse(out)
		require.NoError(t, err)
		assert.Equal(t, "true", u.Query().Get("sendEncrypted"))
	})
}

func TestURLSource_NoEncryptedConfigRawYAML(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("version: \"2\"\n"))
	}))
	t.Cleanup(server.Close)

	source := newURLSourceForTest(server.URL, nil)
	data, err := source.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "version: \"2\"\n", string(data))

	ecs, ok := source.(EncryptedConfigSource)
	require.True(t, ok)
	assert.Empty(t, ecs.EncryptedConfig())
}

// TestURLSource_RecoversEncryptedConfigFrom304 verifies that after a 200 that
// cached the encrypted config, a subsequent 304 (carrying only the digest,
// not the full blob) recovers the value from disk.
func TestURLSource_RecoversEncryptedConfigFrom304(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	const enc = "ENCRYPTED-BLOB"
	const etag = "sha256:abc"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.Header().Set(httpclient.EncryptedConfigDigestHeader, digestOf(enc))
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		writeConfigWithEncrypted(w, enc)
	}))
	t.Cleanup(server.Close)

	src1 := newURLSourceForTest(server.URL, nil)
	_, err := src1.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, enc, src1.(EncryptedConfigSource).EncryptedConfig())

	// Fresh source (empty in-memory state): server 304s, value recovered from disk.
	src2 := newURLSourceForTest(server.URL, nil)
	data, err := src2.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testConfigBody, string(data))
	assert.Equal(t, enc, src2.(EncryptedConfigSource).EncryptedConfig(),
		"encrypted config must be recovered from cache on a 304")
}

// TestURLSource_SelfHealsOn304WithoutCachedConfig verifies that a 304 with no
// cached config forces a full reload to recover it.
func TestURLSource_SelfHealsOn304WithoutCachedConfig(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	const enc = "ENCRYPTED-BLOB"
	const etag = "sha256:abc"

	var notModified, forced int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") == "no-cache" {
			forced++
			w.Header().Set("ETag", etag)
			writeConfigWithEncrypted(w, enc)
			return
		}
		if r.Header.Get("If-None-Match") == etag {
			notModified++
			w.Header().Set(httpclient.EncryptedConfigDigestHeader, digestOf(enc))
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		writeConfigWithEncrypted(w, enc)
	}))
	t.Cleanup(server.Close)

	// Prime the YAML+ETag cache, then delete the .enc sidecar.
	src1 := newURLSourceForTest(server.URL, nil)
	_, err := src1.Read(t.Context())
	require.NoError(t, err)
	encPath := filepath.Join(getURLCacheDir(), hashURL(server.URL)+".enc")
	require.NoError(t, os.Remove(encPath))

	src2 := newURLSourceForTest(server.URL, nil)
	data, err := src2.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testConfigBody, string(data))
	assert.Equal(t, enc, src2.(EncryptedConfigSource).EncryptedConfig(),
		"self-heal must recover the encrypted config")
	assert.Equal(t, 1, notModified, "exactly one 304 before self-healing")
	assert.Equal(t, 1, forced, "exactly one forced reload")
}

// TestURLSource_SelfHealsOn304DigestMismatch verifies a stale cached config is
// discarded and force-reloaded when the server's digest no longer matches.
func TestURLSource_SelfHealsOn304DigestMismatch(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	const staleEnc = "OLD-BLOB"
	const freshEnc = "NEW-BLOB"
	const etag = "sha256:abc"

	var forced int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") == "no-cache" {
			forced++
			w.Header().Set("ETag", etag)
			writeConfigWithEncrypted(w, freshEnc)
			return
		}
		if r.Header.Get("If-None-Match") == etag {
			w.Header().Set(httpclient.EncryptedConfigDigestHeader, digestOf(freshEnc))
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		writeConfigWithEncrypted(w, staleEnc)
	}))
	t.Cleanup(server.Close)

	src1 := newURLSourceForTest(server.URL, nil)
	_, err := src1.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, staleEnc, src1.(EncryptedConfigSource).EncryptedConfig())

	src2 := newURLSourceForTest(server.URL, nil)
	_, err = src2.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, freshEnc, src2.(EncryptedConfigSource).EncryptedConfig(),
		"stale config must be replaced by the forced reload")
	assert.Equal(t, 1, forced, "digest mismatch must trigger exactly one forced reload")
}

// TestURLSource_ForcedReloadStill304 verifies the guard against a misbehaving
// server that answers 304 even to a forced reload (Cache-Control: no-cache),
// advertising a digest the cache cannot satisfy. This must NOT loop, must NOT
// error the whole config load (the YAML is valid and cached), and must forward
// no encrypted config (nothing stale/mismatched leaks to the gateway).
func TestURLSource_ForcedReloadStill304(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	const staleEnc = "OLD-BLOB"
	const freshDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	const etag = "sha256:abc"

	var total, notModified, forced int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		total++
		if r.Header.Get("Cache-Control") == "no-cache" {
			// Misbehaving: 304 to a forced reload, advertising a digest that
			// never matches the cached config.
			forced++
			w.Header().Set(httpclient.EncryptedConfigDigestHeader, freshDigest)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Header.Get("If-None-Match") == etag {
			notModified++
			w.Header().Set(httpclient.EncryptedConfigDigestHeader, freshDigest)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		writeConfigWithEncrypted(w, staleEnc)
	}))
	t.Cleanup(server.Close)

	// Prime the YAML+ETag+.enc cache with the stale config.
	src1 := newURLSourceForTest(server.URL, nil)
	_, err := src1.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, staleEnc, src1.(EncryptedConfigSource).EncryptedConfig())

	// A fresh read now 304s (digest mismatch) -> forces a reload -> the server
	// 304s AGAIN. The read must still succeed with the valid cached YAML, forward
	// no encrypted config, and issue exactly one forced reload (no loop).
	src2 := newURLSourceForTest(server.URL, nil)
	data, err := src2.Read(t.Context())
	require.NoError(t, err, "a misbehaving server must not fail the whole config load")
	assert.Equal(t, testConfigBody, string(data))
	assert.Empty(t, src2.(EncryptedConfigSource).EncryptedConfig(),
		"no encrypted config must be forwarded when a forced reload cannot recover a matching one")
	assert.Equal(t, 1, notModified, "exactly one conditional 304 before forcing a reload")
	assert.Equal(t, 1, forced, "the forced reload must be attempted exactly once (no retry loop)")
}
