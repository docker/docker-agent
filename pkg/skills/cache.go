package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/remote"
)

// remoteHTTPTimeout caps each HTTP request made to a remote skills source.
const remoteHTTPTimeout = 30 * time.Second

// httpGet performs a GET request using the standard remote transport so that
// Docker Desktop proxy/SSL settings are honoured. The returned response body
// must be closed by the caller.
func httpGet(ctx context.Context, url string) (*http.Response, error) {
	client := &http.Client{
		Timeout:   remoteHTTPTimeout,
		Transport: remote.NewTransport(ctx),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request for %s: %w", url, err)
	}
	return client.Do(req)
}

type diskCache struct {
	baseDir string
}

type cacheMetadata struct {
	URL       string    `json:"url"`
	CachedAt  time.Time `json:"cached_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newDiskCache(baseDir string) *diskCache {
	return &diskCache{
		baseDir: baseDir,
	}
}

// cacheDir returns the on-disk directory for a given base URL and skill name.
// Structure: {baseDir}/{urlHash}/{skillName}/
func (c *diskCache) cacheDir(baseURL, skillName string) string {
	h := sha256.Sum256([]byte(baseURL))
	urlHash := hex.EncodeToString(h[:8])
	return filepath.Join(c.baseDir, urlHash, skillName)
}

// Get returns the cached content for a file if it exists and is not expired.
func (c *diskCache) Get(baseURL, skillName, filePath string) (string, bool) {
	dir := c.cacheDir(baseURL, skillName)
	contentPath := filepath.Join(dir, filePath)
	metaPath := contentPath + ".meta"

	meta, err := c.readMetadata(metaPath)
	if err != nil {
		return "", false
	}

	if time.Now().After(meta.ExpiresAt) {
		return "", false
	}

	data, err := os.ReadFile(contentPath)
	if err != nil {
		return "", false
	}

	return string(data), true
}

// FetchAndStore downloads a file from the given URL and stores it in the cache.
// It respects Cache-Control headers to determine expiry.
func (c *diskCache) FetchAndStore(ctx context.Context, baseURL, skillName, filePath, fileURL string) (string, error) {
	slog.DebugContext(ctx, "Fetching remote skill file", "url", fileURL)

	resp, err := httpGet(ctx, fileURL)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", fileURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: HTTP %d", fileURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB per file
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", fileURL, err)
	}

	expiresAt := parseCacheExpiry(resp.Header.Get("Cache-Control"))

	dir := c.cacheDir(baseURL, skillName)
	contentPath := filepath.Join(dir, filePath)
	metaPath := contentPath + ".meta"

	if err := os.MkdirAll(filepath.Dir(contentPath), 0o700); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}

	if err := os.WriteFile(contentPath, body, 0o600); err != nil {
		return "", fmt.Errorf("writing cache file: %w", err)
	}

	meta := cacheMetadata{
		URL:       fileURL,
		CachedAt:  time.Now(),
		ExpiresAt: expiresAt,
	}
	metaJSON, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, metaJSON, 0o600); err != nil {
		// Non-fatal: the content is cached, just the metadata isn't
		slog.DebugContext(ctx, "Failed to write cache metadata", "path", metaPath, "error", err)
	}

	return string(body), nil
}

func (c *diskCache) readMetadata(metaPath string) (cacheMetadata, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return cacheMetadata{}, err
	}
	var meta cacheMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return cacheMetadata{}, err
	}
	return meta, nil
}

const defaultCacheTTL = 1 * time.Hour

// parseCacheExpiry extracts the expiry time from a Cache-Control header value.
// Falls back to defaultCacheTTL if the header is missing or unparseable.
func parseCacheExpiry(cacheControl string) time.Time {
	if cacheControl == "" {
		return time.Now().Add(defaultCacheTTL)
	}

	for directive := range strings.SplitSeq(cacheControl, ",") {
		directive = strings.TrimSpace(directive)

		if strings.EqualFold(directive, "no-store") || strings.EqualFold(directive, "no-cache") {
			// Still cache, but with zero TTL so it's refetched next time
			return time.Now()
		}

		if strings.HasPrefix(strings.ToLower(directive), "max-age=") {
			ageStr := directive[len("max-age="):]
			if seconds, err := strconv.ParseInt(ageStr, 10, 64); err == nil && seconds >= 0 {
				return time.Now().Add(time.Duration(seconds) * time.Second)
			}
		}
	}

	return time.Now().Add(defaultCacheTTL)
}
