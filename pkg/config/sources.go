package config

import (
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
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/remote"
)

type Source interface {
	Name() string
	ParentDir() string
	Read(ctx context.Context) ([]byte, error)
}

type Sources map[string]Source

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

// Read loads an agent configuration from an OCI artifact.
//
// The OCI registry remains the source of truth.
// The local content store is used as a cache and fallback only.
// A forced re-pull is triggered exclusively when store corruption is detected.
func (a ociSource) Read(ctx context.Context) ([]byte, error) {
	store, err := content.NewStore()
	if err != nil {
		return nil, fmt.Errorf("failed to create content store: %w", err)
	}

	// Normalize the reference so that equivalent forms (e.g.
	// "agentcatalog/review-pr" and "index.docker.io/agentcatalog/review-pr:latest")
	// resolve to the same store key that remote.Pull uses.
	storeKey, err := remote.NormalizeReference(a.reference)
	if err != nil {
		return nil, fmt.Errorf("normalizing OCI reference %s: %w", a.reference, err)
	}

	// For digest references, the content is immutable. If we already have
	// the artifact locally, serve it directly without any network call.
	if remote.IsDigestReference(a.reference) {
		if data, loadErr := loadArtifact(store, storeKey); loadErr == nil {
			slog.DebugContext(ctx, "Serving digest-pinned OCI artifact from cache", "ref", a.reference)
			return data, nil
		}
	}

	// Check whether we have a local copy to fall back on.
	hasLocal := hasLocalArtifact(store, storeKey)

	// Pull from registry (checks remote digest, skips download if unchanged).
	if _, pullErr := remote.Pull(ctx, a.reference, false); pullErr != nil {
		if !hasLocal {
			return nil, fmt.Errorf("failed to pull OCI image %s: %w", a.reference, pullErr)
		}
		slog.DebugContext(ctx, "Failed to check for OCI reference updates, using cached version",
			"ref", a.reference, "error", pullErr)
	}

	// Try loading from store.
	data, err := loadArtifact(store, storeKey)
	if err == nil {
		return data, nil
	}

	// If corrupted, force re-pull and try once more.
	if !errors.Is(err, content.ErrStoreCorrupted) {
		return nil, fmt.Errorf("failed to load agent from OCI source %s: %w", a.reference, err)
	}

	slog.WarnContext(ctx, "Local OCI store corrupted, forcing re-pull", "ref", a.reference)
	if _, pullErr := remote.Pull(ctx, a.reference, true); pullErr != nil {
		return nil, fmt.Errorf("failed to force re-pull OCI image %s: %w", a.reference, pullErr)
	}

	data, err = loadArtifact(store, storeKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent from OCI source %s: %w", a.reference, err)
	}
	return data, nil
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

	// Add GitHub token authorization for GitHub URLs
	a.addGitHubAuth(ctx, req)

	client := httpclient.NewHTTPClient(ctx)
	if !a.unsafe {
		client = &http.Client{
			Timeout:       60 * time.Second,
			Transport:     httpclient.NewSSRFSafeTransport(),
			CheckRedirect: httpclient.HTTPSOnlyRedirects(10),
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		// Network error - try to use cached version
		if cachedData, cacheErr := os.ReadFile(cachePath); cacheErr == nil {
			slog.DebugContext(ctx, "Network error fetching URL, using cached version", "url", a.url, "error", err)
			return cachedData, nil
		}
		return nil, fmt.Errorf("fetching %s: %w", a.url, err)
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
		return nil, fmt.Errorf("fetching %s: %s", a.url, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
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

// hashURL creates a safe filename from a URL.
func hashURL(rawURL string) string {
	h := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(h[:])
}

// IsURLReference checks if the input is a valid HTTP/HTTPS URL.
func IsURLReference(input string) bool {
	return strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")
}

// validateAgentURL enforces that an agent URL uses HTTPS. SSRF protection
// (rejecting connections to loopback / private / link-local addresses) is
// done at dial time by [httpclient.NewSSRFSafeTransport] so that DNS
// rebinding cannot be used to bypass it.
func validateAgentURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing to load agent from %q: only https:// URLs are allowed (got scheme %q)", rawURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: missing host", rawURL)
	}
	return nil
}
