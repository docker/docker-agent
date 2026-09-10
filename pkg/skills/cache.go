package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/httpclient"
)

// remoteHTTPTimeout caps each HTTP request made to a remote skills source.
const remoteHTTPTimeout = 30 * time.Second

// skillsHTTPClient is used for outbound calls to remote skill registries.
// The base URL is operator-supplied and the contents are fed to the model
// as instructions, so a hostile (or compromised) registry could otherwise
// be used to read internal endpoints (loopback, RFC1918, link-local incl.
// cloud metadata at 169.254.169.254) and exfiltrate them through prompt
// injection. Remote skill hosts are not in the Docker-owned allowlist, so
// they always use the direct SSRF-guarded path (no Desktop proxy): the
// client refuses such targets at dial time, after DNS resolution, defeating
// DNS rebinding.
//
// Tests in this package replace the var via TestMain (see main_test.go)
// because httptest.NewServer binds to 127.0.0.1.
var skillsHTTPClient = httpclient.NewSafeClient(remoteHTTPTimeout, false)

// httpGet performs a GET request using the SSRF-safe HTTP client. The
// returned response body must be closed by the caller.
func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request for %s: %w", url, err)
	}
	return skillsHTTPClient.Do(req)
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
// Treats missing-file errors as a cache miss (returns false). Other I/O
// errors (e.g. EACCES, corrupt JSON) are surfaced through a debug log so
// they don't masquerade as a benign refetch trigger but still don't break
// the caller — a refetch is the right fallback.
func (c *diskCache) Get(baseURL, skillName, filePath string) (string, bool) {
	dir := c.cacheDir(baseURL, skillName)
	contentPath := filepath.Join(dir, filePath)
	metaPath := contentPath + ".meta"

	meta, err := c.readMetadata(metaPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("Skill cache metadata unreadable, treating as miss", "path", metaPath, "error", err)
		}
		return "", false
	}

	// Stale at ExpiresAt itself (now >= ExpiresAt): zero-freshness entries
	// (max-age=0, no-store, no-cache) set ExpiresAt to the store time, and
	// coarse wall clocks (e.g. Windows; the JSON round-trip drops the
	// monotonic reading) can make Now equal ExpiresAt. Strict After() would
	// wrongly treat that instant as fresh.
	if !time.Now().Before(meta.ExpiresAt) {
		return "", false
	}

	data, err := os.ReadFile(contentPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("Skill cache content unreadable, treating as miss", "path", contentPath, "error", err)
		}
		return "", false
	}

	return string(data), true
}

// FetchAndStore downloads a file from the given URL and stores it in the cache.
// It respects Cache-Control headers to determine expiry: no-cache forces
// immediate expiry, max-age=N sets a TTL of N seconds, and unknown headers
// fall back to defaultCacheTTL.
//
// no-store is treated as "do not retain across Load() cycles": the entry is
// written to disk but with an immediate-expiry marker so the next prefetch
// refetches it. We do not skip the disk write entirely because callers
// (notably the read_skill tool, see pkg/tools/builtin/skills) consume the
// content by re-reading skill.FilePath, not by going through diskCache.Get.
// Skipping the write would render the skill unreadable for the rest of the
// current process. A future improvement is to keep no-store content in an
// in-memory map shared with the reader; until then we trade strict RFC
// 9111 §5.2.2.5 compliance for a working tool.
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

	directive := parseCacheControl(resp.Header.Get("Cache-Control"))

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
		ExpiresAt: directive.expiresAt(),
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

// cacheDirective is the parsed subset of Cache-Control we care about.
type cacheDirective struct {
	noStore bool
	noCache bool
	// hasMaxAge is true when the header explicitly carried max-age=N.
	hasMaxAge bool
	maxAge    time.Duration
}

// expiresAt returns the absolute time after which the cached entry must
// not be reused. no-store and no-cache both force immediate expiry: the
// response may be on disk for the duration of the current process (so the
// in-process reader can consume it), but the next Load() cycle will
// refetch instead of reusing the stored copy. We currently approximate
// no-cache without conditional-GET support; the practical effect is the
// same as no-store with respect to whether the next read sees fresh
// content.
func (d cacheDirective) expiresAt() time.Time {
	now := time.Now()
	if d.noStore || d.noCache {
		return now
	}
	if d.hasMaxAge {
		return now.Add(d.maxAge)
	}
	return now.Add(defaultCacheTTL)
}

// parseCacheControl extracts the directives we honour from a Cache-Control
// header value. Unknown directives are ignored; an empty header yields the
// zero value, which falls back to defaultCacheTTL via expiresAt().
func parseCacheControl(header string) cacheDirective {
	var d cacheDirective
	if header == "" {
		return d
	}

	for directive := range strings.SplitSeq(header, ",") {
		directive = strings.TrimSpace(directive)

		switch {
		case strings.EqualFold(directive, "no-store"):
			d.noStore = true
		case strings.EqualFold(directive, "no-cache"):
			d.noCache = true
		case strings.HasPrefix(strings.ToLower(directive), "max-age="):
			ageStr := directive[len("max-age="):]
			if seconds, err := strconv.ParseInt(ageStr, 10, 64); err == nil && seconds >= 0 {
				d.hasMaxAge = true
				d.maxAge = time.Duration(seconds) * time.Second
			}
		}
	}

	return d
}
