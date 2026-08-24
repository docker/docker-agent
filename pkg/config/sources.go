package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/memoize"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/remote"
)

type Source interface {
	Name() string
	ParentDir() string
	Read(ctx context.Context) ([]byte, error)
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

// ociSource is used to load an agent configuration from an OCI artifact.
type ociSource struct {
	reference string
}

func NewOCISource(reference string) Source {
	return ociSource{
		reference: reference,
	}
}

func (a ociSource) Name() string {
	return a.reference
}

func (a ociSource) ParentDir() string {
	return ""
}

// ociReadMemoizer caches successful OCI reads for a short window, keyed by
// fully-qualified reference (registry host included, so references that
// differ only by registry can never share an entry). A single command
// invocation reads the same source several times (the sandbox-default probe
// in runRunCommand, then the team load), and without memoization each read
// pays a blocking registry digest round-trip even when the artifact is
// cached and unchanged. Failed reads are never cached (memoize retries
// them), and neither are degraded reads that fell back to the local store
// because the registry was unreachable — so recovery is retried on the very
// next read. The TTL bounds staleness for long-lived embedders so a tag
// update is picked up within a minute.
var ociReadMemoizer = memoize.New[[]byte](1 * time.Minute)

// pullOCIArtifact pulls (or digest-checks) an OCI artifact from the registry.
// It is a var so tests can stub the network out.
var pullOCIArtifact = func(ctx context.Context, ref string, force bool) (string, error) {
	return remote.Pull(ctx, ref, force)
}

// degradedReadError smuggles a successful-but-degraded read (registry
// unreachable, stale local copy served) out of the memoized closure as an
// error, so memoize does not cache it. Read unwraps it back into a success.
type degradedReadError struct {
	data []byte
}

func (e *degradedReadError) Error() string {
	return "degraded OCI read served from local store"
}

// Read loads an agent configuration from an OCI artifact.
//
// The OCI registry remains the source of truth.
// The local content store is used as a cache and fallback only.
// A forced re-pull is triggered exclusively when store corruption is detected.
//
// Reads validated against the registry are memoized per process (see
// ociReadMemoizer) so repeated loads of the same reference within one command
// don't repeat the registry round-trip. Concurrent readers of the same
// reference share a single in-flight read — including its context: if the
// first caller is cancelled mid-read, waiters get that error too, but since
// errors are never cached the next read simply retries.
func (a ociSource) Read(ctx context.Context) ([]byte, error) {
	// The memoization key must include the registry host: two references that
	// differ only by registry are distinct trust boundaries and must never
	// share cached bytes.
	cacheKey, err := remote.FullyQualifiedReference(a.reference)
	if err != nil {
		return nil, fmt.Errorf("normalizing OCI reference %s: %w", a.reference, err)
	}

	data, err := ociReadMemoizer.Memoize(cacheKey, func() ([]byte, error) {
		data, degraded, err := a.read(ctx)
		if err != nil {
			return nil, err
		}
		if degraded {
			return nil, &degradedReadError{data: data}
		}
		return data, nil
	})
	if degraded, ok := errors.AsType[*degradedReadError](err); ok {
		return degraded.data, nil
	}
	if err != nil {
		return nil, err
	}
	// Clone so no caller can mutate the cached bytes shared with later reads.
	return bytes.Clone(data), nil
}

// read performs the actual (unmemoized) load of the OCI artifact. degraded
// reports that the returned data came from the local store because the
// registry could not be reached — a success for the caller, but one that
// must not be cached so the next read retries the registry.
func (a ociSource) read(ctx context.Context) (data []byte, degraded bool, err error) {
	store, err := content.NewStore()
	if err != nil {
		return nil, false, fmt.Errorf("failed to create content store: %w", err)
	}

	// Normalize the reference so that equivalent forms (e.g.
	// "myorg/review-pr" and "index.docker.io/myorg/review-pr:latest")
	// resolve to the same store key that remote.Pull uses.
	storeKey, err := remote.NormalizeReference(a.reference)
	if err != nil {
		return nil, false, fmt.Errorf("normalizing OCI reference %s: %w", a.reference, err)
	}

	// For digest references, the content is immutable. If we already have
	// the artifact locally, serve it directly without any network call.
	if remote.IsDigestReference(a.reference) {
		if data, loadErr := loadArtifact(store, storeKey); loadErr == nil {
			slog.DebugContext(ctx, "Serving digest-pinned OCI artifact from cache", "ref", a.reference)
			return data, false, nil
		}
	}

	// Check whether we have a local copy to fall back on.
	hasLocal := hasLocalArtifact(store, storeKey)

	// Pull from registry (checks remote digest, skips download if unchanged).
	if _, pullErr := pullOCIArtifact(ctx, a.reference, false); pullErr != nil {
		if !hasLocal {
			return nil, false, fmt.Errorf("failed to pull OCI image %s: %w", a.reference, pullErr)
		}
		slog.DebugContext(ctx, "Failed to check for OCI reference updates, using cached version",
			"ref", a.reference, "error", pullErr)
		degraded = true
	}

	// Try loading from store.
	data, err = loadArtifact(store, storeKey)
	if err == nil {
		return data, degraded, nil
	}

	// If corrupted, force re-pull and try once more.
	if !errors.Is(err, content.ErrStoreCorrupted) {
		return nil, false, fmt.Errorf("failed to load agent from OCI source %s: %w", a.reference, err)
	}

	slog.WarnContext(ctx, "Local OCI store corrupted, forcing re-pull", "ref", a.reference)
	if _, pullErr := pullOCIArtifact(ctx, a.reference, true); pullErr != nil {
		return nil, false, fmt.Errorf("failed to force re-pull OCI image %s: %w", a.reference, pullErr)
	}

	data, err = loadArtifact(store, storeKey)
	if err != nil {
		return nil, false, fmt.Errorf("failed to load agent from OCI source %s: %w", a.reference, err)
	}
	return data, false, nil
}

// loadArtifact reads the agent YAML from the content store.
func loadArtifact(store *content.Store, storeKey string) ([]byte, error) {
	af, err := store.GetArtifact(storeKey)
	if err != nil {
		return nil, err
	}
	return []byte(af), nil
}

// hasLocalArtifact reports whether the content store has metadata for the given key.
func hasLocalArtifact(store *content.Store, storeKey string) bool {
	_, err := store.GetArtifactMetadata(storeKey)
	return err == nil
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
}

// NewURLSource creates a new URL source. If envProvider is non-nil, it will be used
// to look up GITHUB_TOKEN for authentication when fetching from GitHub URLs.
func NewURLSource(rawURL string, envProvider environment.Provider) Source {
	return &urlSource{
		url:         rawURL,
		envProvider: envProvider,
	}
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

	// Read cached ETag if available
	cachedETag := ""
	if etagData, err := os.ReadFile(etagPath); err == nil {
		cachedETag = string(etagData)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Include If-None-Match header if we have a cached ETag
	if cachedETag != "" {
		req.Header.Set("If-None-Match", cachedETag)
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
		if cachedData, cacheErr := os.ReadFile(cachePath); cacheErr == nil {
			slog.DebugContext(ctx, "Network error fetching URL, using cached version", "url", a.url, "error", err)
			return cachedData, nil
		}
		return nil, fmt.Errorf("%w: fetching %s: %w", ErrSourceFetchFailed, a.url, err)
	}
	defer resp.Body.Close()

	// 304 Not Modified - return cached content
	if resp.StatusCode == http.StatusNotModified {
		if cachedData, cacheErr := os.ReadFile(cachePath); cacheErr == nil {
			slog.DebugContext(ctx, "URL not modified, using cached version", "url", a.url)
			return cachedData, nil
		}
		// Cache file missing despite 304, fall through to fetch again
	}

	if resp.StatusCode != http.StatusOK {
		// HTTP error - try to use cached version
		if cachedData, cacheErr := os.ReadFile(cachePath); cacheErr == nil {
			slog.DebugContext(ctx, "HTTP error fetching URL, using cached version", "url", a.url, "status", resp.Status)
			return cachedData, nil
		}
		return nil, fmt.Errorf("%w: fetching %s: %s", ErrSourceFetchFailed, a.url, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading response body: %w", ErrSourceFetchFailed, err)
	}

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
