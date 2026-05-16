package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/remote"
)

const (
	DockerCatalogURL     = "https://desktop.docker.com/mcp/catalog/v3/catalog.yaml"
	catalogCacheFileName = "mcp_catalog.json"
	fetchTimeout         = 15 * time.Second

	// catalogJSON is the URL we actually fetch (JSON is ~3x faster to parse than YAML).
	catalogJSON = "https://desktop.docker.com/mcp/catalog/v3/catalog.json"
)

func RequiredEnvVars(ctx context.Context, serverName string) ([]Secret, error) {
	server, err := ServerSpec(ctx, serverName)
	if err != nil {
		return nil, err
	}

	// TODO(dga): until the MCP Gateway supports oauth with docker agent,
	// we ignore every secret listed on `remote` servers and assume
	// we can use oauth by connecting directly to the server's url.
	if server.Type == "remote" {
		return nil, nil
	}

	return server.Secrets, nil
}

func ServerSpec(_ context.Context, serverName string) (Server, error) {
	catalog, err := catalogOnce()
	if err != nil {
		return Server{}, err
	}

	server, ok := catalog[serverName]
	if !ok {
		return Server{}, fmt.Errorf("MCP server %q not found in MCP catalog", serverName)
	}

	return server, nil
}

// ParseServerRef strips the optional "docker:" prefix from a server reference.
func ParseServerRef(ref string) string {
	return strings.TrimPrefix(ref, "docker:")
}

// cachedCatalog is the on-disk cache format.
type cachedCatalog struct {
	Catalog Catalog `json:"catalog"`
	ETag    string  `json:"etag,omitempty"`
}

// catalogOnce guards one-shot catalog loading.
// We use sync.OnceValues so that:
//   - the catalog is fetched at most once per process, and
//   - we detach from the caller's context to avoid permanently
//     caching a context-cancellation error.
var catalogOnce = sync.OnceValues(func() (Catalog, error) {
	return fetchAndCache(context.Background())
})

// fetchAndCache tries to fetch the catalog from the network (using ETag for
// conditional requests) and falls back to the disk cache on any failure.
func fetchAndCache(ctx context.Context) (Catalog, error) {
	cacheFile := cacheFilePath()
	cached := loadFromDisk(cacheFile)

	catalog, newETag, err := fetchFromNetwork(ctx, cached.ETag)
	if err != nil {
		slog.DebugContext(ctx, "Failed to fetch MCP catalog from network, using cache", "error", err)
		if cached.Catalog != nil {
			return cached.Catalog, nil
		}
		return nil, fmt.Errorf("fetching MCP catalog: %w (no cached copy available)", err)
	}

	// A nil catalog means 304 Not Modified — the cached copy is still valid.
	if catalog == nil {
		slog.DebugContext(ctx, "MCP catalog not modified (ETag match)")
		return cached.Catalog, nil
	}

	slog.DebugContext(ctx, "MCP catalog fetched from network")
	saveToDisk(cacheFile, catalog, newETag)

	return catalog, nil
}

func cacheFilePath() string {
	return filepath.Join(paths.GetCacheDir(), catalogCacheFileName)
}

func loadFromDisk(path string) cachedCatalog {
	data, err := os.ReadFile(path)
	if err != nil {
		return cachedCatalog{}
	}

	var cached cachedCatalog
	if err := json.Unmarshal(data, &cached); err != nil {
		return cachedCatalog{}
	}

	return cached
}

func saveToDisk(path string, catalog Catalog, etag string) {
	data, err := json.Marshal(cachedCatalog{Catalog: catalog, ETag: etag})
	if err != nil {
		slog.Warn("Failed to marshal MCP catalog cache", "error", err)
		return
	}

	dir := filepath.Dir(path)

	// Write to a temp file and rename so readers never see a partial file.
	// Try creating the temp file first; only create the directory if needed.
	tmp, err := os.CreateTemp(dir, ".mcp_catalog_*.tmp")
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("Failed to create MCP catalog temp file", "error", err)
			return
		}
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil { //nolint:gosec // shared with other docker MCP gateway processes
			slog.Warn("Failed to create MCP catalog cache directory", "error", mkErr)
			return
		}
		tmp, err = os.CreateTemp(dir, ".mcp_catalog_*.tmp")
		if err != nil {
			slog.Warn("Failed to create MCP catalog temp file", "error", err)
			return
		}
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		slog.Warn("Failed to write MCP catalog temp file", "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		slog.Warn("Failed to close MCP catalog temp file", "error", err)
		return
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		slog.Warn("Failed to rename MCP catalog cache file", "error", err)
	}
}

// fetchFromNetwork fetches the catalog, using the ETag for conditional requests.
// It returns (nil, "", nil) when the server responds with 304 Not Modified.
func fetchFromNetwork(ctx context.Context, etag string) (Catalog, string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogJSON, http.NoBody)
	if err != nil {
		return nil, "", err
	}

	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	catalogClient := &http.Client{Transport: remote.NewTransport(ctx)}
	resp, err := catalogClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, "", nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected status fetching MCP catalog: %s", resp.Status)
	}

	var top topLevel
	if err := json.NewDecoder(resp.Body).Decode(&top); err != nil {
		return nil, "", err
	}

	return top.Catalog, resp.Header.Get("ETag"), nil
}
