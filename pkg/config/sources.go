package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/configsize"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/paths"
)

type Source interface {
	Name() string
	ParentDir() string
	Read(ctx context.Context) ([]byte, error)
}

// EncryptedConfigSource is implemented by sources that may discover an
// encrypted representation of the agent config while reading it (for example a
// trusted Docker URL that returns the X-Cagent-Encrypted-Config response
// header). Callers can type-assert a Source to this interface after Read to
// pick up the value, which is then forwarded to a trusted Docker models gateway
// on subsequent model requests.
type EncryptedConfigSource interface {
	// EncryptedConfig returns the encrypted config captured during the most
	// recent successful Read, or "" if none was seen. Safe to call concurrently.
	EncryptedConfig() string
}

type Sources map[string]Source

// ErrSourceFetchFailed reports that a remote URL source could not be fetched.
// It intentionally excludes URL validation and config parsing errors.
var ErrSourceFetchFailed = errors.New("remote source fetch failed")

// fileSource is used to load an agent configuration from a YAML file.
type fileSource struct {
	path string
}

func NewFileSource(path string) Source {
	return fileSource{
		path: path,
	}
}

func (a fileSource) Name() string {
	return a.path
}

func (a fileSource) ParentDir() string {
	return filepath.Dir(a.path)
}

func (a fileSource) Read(context.Context) ([]byte, error) {
	parentDir := a.ParentDir()
	fs, err := os.OpenRoot(parentDir)
	if err != nil {
		return nil, fmt.Errorf("opening filesystem %s: %w", parentDir, err)
	}
	defer fs.Close()

	fileName := filepath.Base(a.path)
	data, err := fs.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", fileName, err)
	}

	return data, nil
}

// bytesSource is used to load an agent configuration from a []byte.
type bytesSource struct {
	name string
	data []byte
}

func NewBytesSource(name string, data []byte) Source {
	return bytesSource{
		name: name,
		data: data,
	}
}

func (a bytesSource) Name() string {
	return a.name
}

func (a bytesSource) ParentDir() string {
	return ""
}

func (a bytesSource) Read(context.Context) ([]byte, error) {
	return a.data, nil
}

// urlSource is used to load an agent configuration from an HTTP/HTTPS URL.
type urlSource struct {
	url         string
	envProvider environment.Provider
	// unsafe disables the HTTPS-only and SSRF dial-time checks. It is set
	// only by the test-only constructor newURLSourceForTest (defined in
	// sources_test.go), which exists because tests use httptest.NewServer
	// (plain HTTP, 127.0.0.1).
	unsafe bool
	// encryptedConfig captures the X-Cagent-Encrypted-Config response header
	// seen on a successful fetch from a trusted Docker URL. It is a pointer so
	// the value-receiver Read can persist the captured value back onto the
	// pointer the caller holds, and so concurrent readers see a consistent
	// value.
	encryptedConfig *atomic.Pointer[string]
}

// NewURLSource creates a new URL source. If envProvider is non-nil, it will be used
// to look up GITHUB_TOKEN for authentication when fetching from GitHub URLs.
func NewURLSource(rawURL string, envProvider environment.Provider) Source {
	return &urlSource{
		url:             rawURL,
		envProvider:     envProvider,
		encryptedConfig: &atomic.Pointer[string]{},
	}
}

// EncryptedConfig returns the encrypted agent config captured from the
// X-Cagent-Encrypted-Config response header during the most recent successful
// Read from a trusted Docker URL, or "" if none was seen. It implements
// [EncryptedConfigSource].
func (a urlSource) EncryptedConfig() string {
	if a.encryptedConfig == nil {
		return ""
	}
	if v := a.encryptedConfig.Load(); v != nil {
		return *v
	}
	return ""
}

// storeEncryptedConfig records enc both in memory (for this process) and on
// disk (encPath, next to the cached YAML) so a later run that revalidates the
// URL and gets a 304 — which no longer carries the full config, only its digest
// — can recover the value from disk instead of the response.
func (a urlSource) storeEncryptedConfig(ctx context.Context, enc, encPath string) {
	if a.encryptedConfig != nil {
		a.encryptedConfig.Store(&enc)
	}
	// Ensure the cache directory exists: captureEncryptedConfigFromEnvelope may run before
	// the YAML-caching block that creates it (e.g. this fetch returns early).
	if err := os.MkdirAll(filepath.Dir(encPath), 0o700); err != nil {
		slog.DebugContext(ctx, "Failed to create cache dir for encrypted agent config", "url", a.url, "error", err)
		return
	}
	if err := os.WriteFile(encPath, []byte(enc), 0o600); err != nil {
		slog.DebugContext(ctx, "Failed to cache encrypted agent config", "url", a.url, "error", err)
	}
}

// clearEncryptedConfig drops any in-memory encrypted config so that, until the
// next successful capture, no (potentially stale) value is forwarded to the
// gateway. The on-disk sidecar is left untouched. Safe to call concurrently.
func (a urlSource) clearEncryptedConfig() {
	if a.encryptedConfig != nil {
		a.encryptedConfig.Store(nil)
	}
}

// adoptCachedEncryptedConfig loads the encrypted config cached on disk and, if
// present, records it in memory so it is forwarded to the Docker models gateway
// on subsequent model requests. When wantDigest is non-empty (the server sent
// X-Cagent-Encrypted-Config-Digest on a 304), the cached value is only adopted
// if its digest matches, so a stale cache is never replayed. It reports whether
// a usable, matching value was adopted.
func (a urlSource) adoptCachedEncryptedConfig(ctx context.Context, encPath, wantDigest string) bool {
	if a.encryptedConfig == nil {
		return false
	}
	if !isTrustedDockerURL(a.url) {
		return false
	}
	data, err := os.ReadFile(encPath)
	if err != nil || len(data) == 0 {
		return false
	}
	enc := string(data)
	if wantDigest != "" && encryptedConfigDigest(enc) != wantDigest {
		slog.DebugContext(ctx, "Cached encrypted agent config does not match server digest; discarding", "url", a.url)
		return false
	}
	a.encryptedConfig.Store(&enc)
	slog.DebugContext(ctx, "Recovered encrypted agent config from cache", "url", a.url)
	return true
}

// captureEncryptedConfigFromBody, when body is agent YAML from a trusted
// Docker URL that carries a top-level `encrypted_agent_config` field, records
// the encrypted config (in memory and on encPath) and returns the body with
// that field stripped, so the config the rest of the pipeline parses never sees
// it. For any other body (no such field, non-YAML, or an untrusted host) it
// returns body unchanged and captures nothing. The trust check mirrors the
// Docker JWT injection so the value is never captured from an untrusted host.
func (a urlSource) captureEncryptedConfigFromBody(ctx context.Context, body, encPath string) string {
	if !isTrustedDockerURL(a.url) {
		return body
	}

	var doc yaml.MapSlice
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		// Not a YAML mapping (or malformed); leave it for the strict parser to
		// report a precise error later.
		return body
	}

	enc := ""
	stripped := make(yaml.MapSlice, 0, len(doc))
	for _, item := range doc {
		if key, ok := item.Key.(string); ok && key == httpclient.EncryptedConfigBodyField {
			if s, ok := item.Value.(string); ok {
				enc = s
			}
			continue
		}
		stripped = append(stripped, item)
	}
	if enc == "" {
		return body
	}

	if a.encryptedConfig != nil {
		a.storeEncryptedConfig(ctx, enc, encPath)
	}
	slog.DebugContext(ctx, "Captured encrypted agent config from Docker source YAML field", "url", a.url)

	out, err := yaml.Marshal(stripped)
	if err != nil {
		// Should not happen; fall back to the original body so the agent still
		// loads (the field will surface as an unknown-field parse error, which
		// is a safe, visible failure rather than silent corruption).
		slog.DebugContext(ctx, "Failed to re-encode agent YAML after stripping encrypted config", "url", a.url, "error", err)
		return body
	}
	return string(out)
}

// sendEncryptedQueryParam is the query parameter docker-agent adds to a trusted
// Docker config-fetch URL to opt in to receiving the encrypted agent config
// embedded in the response body. It must stay in sync with the Docker gateway
// (gordon proxy pkg/agents/handler.go), which only injects the config when it
// is set to "true".
const sendEncryptedQueryParam = "sendEncrypted"

// urlWithSendEncrypted returns rawURL with sendEncrypted=true added to its
// query string, preserving any existing parameters. An existing sendEncrypted
// value is overwritten.
func urlWithSendEncrypted(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set(sendEncryptedQueryParam, "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// encryptedConfigDigest returns the "sha256:<hex>" fingerprint of enc, matching
// the format a trusted Docker source sends in X-Cagent-Encrypted-Config-Digest.
func encryptedConfigDigest(enc string) string {
	if enc == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(enc))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (a urlSource) Name() string {
	return a.url
}

func (a urlSource) ParentDir() string {
	return ""
}

// getURLCacheDir returns the directory used to cache URL-based agent configurations.
func getURLCacheDir() string {
	return filepath.Join(paths.GetDataDir(), "url_cache")
}

func (a urlSource) Read(ctx context.Context) ([]byte, error) {
	if !a.unsafe {
		if err := validateAgentURL(a.url); err != nil {
			return nil, err
		}
	}

	cacheDir := getURLCacheDir()
	urlHash := hashURL(a.url)
	cachePath := filepath.Join(cacheDir, urlHash)
	etagPath := cachePath + ".etag"
	// encPath sidecars the cached YAML with the encrypted agent config captured
	// from the X-Cagent-Encrypted-Config header on a 200, so a later 304 (which
	// carries only the digest) can recover it.
	encPath := cachePath + ".enc"

	return a.read(ctx, cacheDir, cachePath, etagPath, encPath, false)
}

// read performs the conditional fetch and caching. When forceReload is true it
// skips the If-None-Match revalidation so the server answers with a fresh 200
// (never a 304); this is the self-healing path taken when a 304 arrives but the
// encrypted config could not be recovered from cache (missing or digest
// mismatch).
func (a urlSource) read(ctx context.Context, cacheDir, cachePath, etagPath, encPath string, forceReload bool) ([]byte, error) {
	// Read cached ETag if available (skipped when forcing a reload).
	cachedETag := ""
	if !forceReload {
		if etagData, err := os.ReadFile(etagPath); err == nil {
			cachedETag = string(etagData)
		}
	}

	reqURL := a.url
	if isTrustedDockerURL(a.url) {
		// Opt in to receiving the encrypted agent config embedded in the
		// response body. Only trusted Docker gateways honor this, and only a
		// client that sets it (like us) knows to strip the injected field
		// before parsing — so the flag both requests the value and signals we
		// can handle it. Best-effort: a malformed URL falls back to the
		// original and simply won't receive the config.
		if withFlag, err := urlWithSendEncrypted(a.url); err == nil {
			reqURL = withFlag
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Include If-None-Match header if we have a cached ETag and are not forcing
	// a full reload.
	if cachedETag != "" {
		req.Header.Set("If-None-Match", cachedETag)
	}
	// On a forced reload, ask any intermediary (and the origin) not to answer
	// from cache, so we are guaranteed a fresh 200 carrying the full encrypted
	// config rather than another 304.
	if forceReload {
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
	}

	a.addGitHubAuth(ctx, req)
	a.addDockerAuth(ctx, req)

	client := httpclient.NewHTTPClient(ctx)
	if !a.unsafe {
		if isLocalhostHTTP(a.url) {
			client = &http.Client{
				Timeout:       60 * time.Second,
				CheckRedirect: httpclient.LocalhostOnlyRedirects(10),
			}
		} else {
			client = &http.Client{
				Timeout:       60 * time.Second,
				Transport:     httpclient.NewDesktopAwareSSRFSafeTransport(),
				CheckRedirect: httpclient.HTTPSOnlyRedirects(10),
			}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		// Network error - try to use cached version
		if cachedData, cacheErr := readCachedConfig(cachePath); cacheErr == nil {
			slog.DebugContext(ctx, "Network error fetching URL, using cached version", "url", a.url, "error", err)
			a.adoptCachedEncryptedConfig(ctx, encPath, "")
			return cachedData, nil
		} else if errors.Is(cacheErr, configsize.ErrTooLarge) {
			return nil, fmt.Errorf("reading cached configuration for %s: %w", a.url, cacheErr)
		}
		return nil, fmt.Errorf("%w: fetching %s: %w", ErrSourceFetchFailed, a.url, err)
	}
	defer resp.Body.Close()

	// The full encrypted agent config now rides in the 200 response body
	// envelope (handled after the body is read below); a 304 carries only the
	// digest, handled next.

	// 304 Not Modified - return cached content
	if resp.StatusCode == http.StatusNotModified {
		if cachedData, cacheErr := readCachedConfig(cachePath); cacheErr == nil {
			// Recover the encrypted config from disk, verifying it against the
			// digest the server sent (when present). If it is missing or stale,
			// self-heal by forcing a full reload so we get the config again.
			wantDigest := resp.Header.Get(httpclient.EncryptedConfigDigestHeader)
			if wantDigest != "" && !a.adoptCachedEncryptedConfig(ctx, encPath, wantDigest) {
				if !forceReload {
					slog.DebugContext(ctx, "304 received but cached encrypted config is missing or stale; forcing a reload", "url", a.url)
					return a.read(ctx, cacheDir, cachePath, etagPath, encPath, true)
				}
				// We already forced a reload (Cache-Control: no-cache), yet the
				// server answered 304 again with a digest we still cannot match
				// from cache. A forced reload is supposed to bypass caching and
				// return a fresh 200 carrying the full config, so a second 304 is
				// a server/protocol violation we cannot recover from here.
				//
				// The YAML config itself is valid and cached, so we do NOT fail
				// the whole config load over it — that would keep the agent from
				// starting for what is only an encrypted-config-forwarding issue.
				// Instead we leave the encrypted config unset (nothing stale or
				// mismatched is forwarded to the gateway; the proxy-side check
				// fails open for a missing header) and warn loudly.
				a.clearEncryptedConfig()
				slog.WarnContext(ctx, "Server returned 304 to a forced reload but the cached encrypted agent config does not match the advertised digest; forwarding no encrypted config for this URL", "url", a.url, "want_digest", wantDigest)
			}
			if wantDigest == "" {
				// No digest advertised: best-effort adopt whatever is cached.
				a.adoptCachedEncryptedConfig(ctx, encPath, "")
			}
			slog.DebugContext(ctx, "URL not modified, using cached version", "url", a.url)
			return cachedData, nil
		} else if errors.Is(cacheErr, configsize.ErrTooLarge) {
			return nil, fmt.Errorf("reading cached configuration for %s: %w", a.url, cacheErr)
		}
		// Cache file missing despite 304, fall through to fetch again
	}

	if resp.StatusCode != http.StatusOK {
		// HTTP error - try to use cached version
		if cachedData, cacheErr := readCachedConfig(cachePath); cacheErr == nil {
			slog.DebugContext(ctx, "HTTP error fetching URL, using cached version", "url", a.url, "status", resp.Status)
			a.adoptCachedEncryptedConfig(ctx, encPath, "")
			return cachedData, nil
		} else if errors.Is(cacheErr, configsize.ErrTooLarge) {
			return nil, fmt.Errorf("reading cached configuration for %s: %w", a.url, cacheErr)
		}
		return nil, fmt.Errorf("%w: fetching %s: %s", ErrSourceFetchFailed, a.url, resp.Status)
	}

	data, err := configsize.Read(resp.Body)
	if err != nil {
		if errors.Is(err, configsize.ErrTooLarge) {
			return nil, fmt.Errorf("fetching %s: %w", a.url, err)
		}
		return nil, fmt.Errorf("%w: reading response body: %w", ErrSourceFetchFailed, err)
	}

	// Cache the response and any associated encrypted config only after the
	// complete body passes the size limit. When a trusted Docker source embeds
	// the encrypted config as a top-level YAML field, capture it and strip the
	// field so the cached content is the plain agent YAML (raw-YAML sources and
	// untrusted hosts are returned unchanged).
	data = []byte(a.captureEncryptedConfigFromBody(ctx, string(data), encPath))

	// Cache the response
	if err := os.MkdirAll(cacheDir, 0o700); err == nil {
		if err := os.WriteFile(cachePath, data, 0o600); err != nil {
			slog.DebugContext(ctx, "Failed to cache URL content", "url", a.url, "error", err)
		}

		// Save ETag if present
		if etag := resp.Header.Get("ETag"); etag != "" {
			if err := os.WriteFile(etagPath, []byte(etag), 0o600); err != nil {
				slog.DebugContext(ctx, "Failed to cache ETag", "url", a.url, "error", err)
			}
		} else {
			// Remove stale ETag file if server no longer provides ETag
			_ = os.Remove(etagPath)
		}
	}

	return data, nil
}

func readCachedConfig(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := configsize.Read(file)
	return data, errors.Join(readErr, file.Close())
}

// githubHosts lists the hostnames that support GitHub token authentication.
var githubHosts = []string{
	"github.com",
	"raw.githubusercontent.com",
	"gist.githubusercontent.com",
}

// isGitHubURL checks if the URL is a GitHub URL that can use token authentication.
// It performs strict hostname validation to prevent token leakage to malicious domains.
func isGitHubURL(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	return slices.Contains(githubHosts, u.Host)
}

// isTrustedDockerURL is an alias for [environment.IsTrustedDockerURL] for local readability.
var isTrustedDockerURL = environment.IsTrustedDockerURL

// addGitHubAuth adds GitHub token authorization to the request if:
// - The URL is a GitHub URL
// - An environment provider is configured
// - GITHUB_TOKEN is available in the environment
func (a urlSource) addGitHubAuth(ctx context.Context, req *http.Request) {
	if a.envProvider == nil {
		return
	}

	if !isGitHubURL(a.url) {
		return
	}

	token, ok := a.envProvider.Get(ctx, "GITHUB_TOKEN")
	if !ok || token == "" {
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)
	slog.DebugContext(ctx, "Added GitHub token authorization to request", "url", a.url)
}

// addDockerAuth adds the Docker Desktop JWT to the request if:
// - The URL targets a .docker.com domain or localhost
// - An environment provider is configured
// - DOCKER_TOKEN is available in the environment
func (a urlSource) addDockerAuth(ctx context.Context, req *http.Request) {
	if a.envProvider == nil {
		return
	}

	if !isTrustedDockerURL(a.url) {
		return
	}

	token, ok := a.envProvider.Get(ctx, environment.DockerDesktopTokenEnv)
	if !ok || token == "" {
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)
	slog.DebugContext(ctx, "Added Docker token authorization to request", "url", a.url)
}

// hashURL creates a safe filename from a URL.
func hashURL(rawURL string) string {
	h := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(h[:])
}

// IsURLReference checks if the input is a valid HTTP/HTTPS URL.
func IsURLReference(input string) bool {
	return strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")
}

// isLocalhostHTTP reports whether rawURL is an http:// URL targeting localhost.
func isLocalhostHTTP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" && isLocalhostHTTPHost(u.Hostname())
}

func isLocalhostHTTPHost(host string) bool {
	// Deliberately exact: this predicate decides whether an agent source may
	// be fetched over plaintext HTTP with no dial-time SSRF guard (see
	// urlSource.Read). *.localhost is an RFC 6761 convention, not guaranteed
	// by musl, Docker's embedded DNS (127.0.0.11), or corporate resolvers.
	// Use httpclient.isLoopbackHost for transport-level checks.
	return strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
}

// validateAgentURL enforces that an agent URL uses HTTPS, with an exception
// for http://localhost which is allowed for local development. SSRF protection
// is applied by [httpclient.NewDesktopAwareSSRFSafeTransport]: on the direct
// path (non-Docker host or Desktop unavailable) it refuses non-public IPs at
// dial time after DNS resolution; Docker-owned hosts may go through Desktop's
// PAC proxy where dial-time enforcement does not apply. The SSRF transport is
// intentionally skipped for http://localhost since loopback is the whole point.
func validateAgentURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" && !isLocalhostHTTP(rawURL) {
		return fmt.Errorf("refusing to load agent from %q: only https:// URLs are allowed (got scheme %q)", rawURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: missing host", rawURL)
	}
	return nil
}
