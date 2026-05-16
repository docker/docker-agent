package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/remote"
)

const (
	ModelsDevAPIURL = "https://models.dev/api.json"
	CacheFileName   = "models_dev.json"
	refreshInterval = 24 * time.Hour
)

// Store manages access to the models.dev data.
// All methods are safe for concurrent use.
//
// The database is loaded on first access via GetDatabase and
// then cached in memory for the lifetime of the Store.
type Store struct {
	cacheFile string
	mu        sync.Mutex
	db        *Database
}

// Opt configures a Store created with NewStore.
type Opt func(*storeOptions)

type storeOptions struct {
	cacheFile string
}

// WithCache overrides the path of the on-disk cache file used by the Store.
// The parent directory will be created if it does not already exist.
func WithCache(path string) Opt {
	return func(o *storeOptions) {
		o.cacheFile = path
	}
}

// NewStore creates a new Store backed by an on-disk cache. By default the
// cache lives at ~/.cagent/models_dev.json; use WithCache to override the
// location.
// Callers should create one Store and share it rather than calling NewStore
// repeatedly. RuntimeConfig.ModelsDevStore() is the standard way to obtain
// a shared instance.
func NewStore(opts ...Opt) (*Store, error) {
	var options storeOptions
	for _, opt := range opts {
		opt(&options)
	}

	cacheFile := options.cacheFile
	if cacheFile == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get user home directory: %w", err)
		}
		cacheFile = filepath.Join(homeDir, ".cagent", CacheFileName)
	}

	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	return &Store{
		cacheFile: cacheFile,
	}, nil
}

// NewDatabaseStore creates a Store pre-populated with the given database.
// The returned store serves data entirely from memory and never fetches
// from the network or touches the filesystem, making it suitable for
// tests and any scenario where the provider data is already known.
func NewDatabaseStore(db *Database) *Store {
	return &Store{db: db}
}

// GetDatabase returns the models.dev database, fetching from cache or API as needed.
func (s *Store) GetDatabase(ctx context.Context) (*Database, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		return s.db, nil
	}

	db, err := loadDatabase(ctx, s.cacheFile)
	if err != nil {
		return nil, err
	}

	s.db = db
	return db, nil
}

// getProvider returns a specific provider by ID.
func (s *Store) getProvider(ctx context.Context, providerID string) (*Provider, error) {
	db, err := s.GetDatabase(ctx)
	if err != nil {
		return nil, err
	}

	provider, exists := db.Providers[providerID]
	if !exists {
		return nil, fmt.Errorf("provider %q not found", providerID)
	}

	return &provider, nil
}

// GetModel returns a specific model by ID. The ID must carry both a
// provider and a model component; pass the result of [NewID], [ParseID],
// or a provider's [ID] method.
func (s *Store) GetModel(ctx context.Context, id ID) (*Model, error) {
	if !id.IsValid() {
		return nil, fmt.Errorf("invalid model ID: %q", id.String())
	}

	provider, err := s.getProvider(ctx, id.Provider)
	if err != nil {
		return nil, err
	}

	model, exists := provider.Models[id.Model]

	// For amazon-bedrock, try stripping region/inference profile prefixes.
	// Bedrock uses prefixes for cross-region inference profiles,
	// but models.dev stores models without these prefixes.
	if !exists && id.Provider == "amazon-bedrock" {
		if prefix, after, ok := strings.Cut(id.Model, "."); ok && bedrockRegionPrefixes[prefix] {
			model, exists = provider.Models[after]
		}
	}

	if !exists {
		return nil, fmt.Errorf("model %q not found in provider %q", id.Model, id.Provider)
	}

	return &model, nil
}

// loadDatabase loads the database from the local cache file or
// falls back to fetching from the models.dev API.
func loadDatabase(ctx context.Context, cacheFile string) (*Database, error) {
	// Try to load from cache first
	cached, err := loadFromCache(cacheFile)
	if err == nil && time.Since(cached.LastRefresh) < refreshInterval {
		return &cached.Database, nil
	}

	// Cache is stale or doesn't exist — try a conditional fetch with the ETag.
	var etag string
	if cached != nil {
		etag = cached.ETag
	}

	database, newETag, fetchErr := fetchFromAPI(ctx, etag)
	if fetchErr != nil {
		// If API fetch fails but we have cached data, use it regardless of age.
		if cached != nil {
			slog.DebugContext(ctx, "API fetch failed, using stale cache", "error", fetchErr)
			return &cached.Database, nil
		}
		return nil, fmt.Errorf("failed to fetch from API and no cached data available: %w", fetchErr)
	}

	// database is nil when the server returned 304 Not Modified.
	if database == nil && cached != nil {
		// Bump LastRefresh so we don't re-check until the next interval.
		cached.LastRefresh = time.Now()
		if saveErr := saveToCache(cacheFile, &cached.Database, cached.ETag); saveErr != nil {
			slog.WarnContext(ctx, "Failed to update cache timestamp", "error", saveErr)
		}
		return &cached.Database, nil
	}

	// Save the fresh data to cache.
	if saveErr := saveToCache(cacheFile, database, newETag); saveErr != nil {
		slog.WarnContext(ctx, "Failed to save to cache", "error", saveErr)
	}

	return database, nil
}

// fetchFromAPI fetches the models.dev database.
// If etag is non-empty it is sent as If-None-Match; a 304 response
// returns (nil, etag, nil) to indicate no change.
func fetchFromAPI(ctx context.Context, etag string) (*Database, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevAPIURL, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}

	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: remote.NewTransport(ctx)}).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch from API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		slog.DebugContext(ctx, "models.dev data not modified (304)")
		return nil, etag, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	// Read the full body then unmarshal — avoids the extra intermediate
	// buffering that json.Decoder.Decode performs.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read response body: %w", err)
	}

	var providers map[string]Provider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, "", fmt.Errorf("failed to decode response: %w", err)
	}

	newETag := resp.Header.Get("ETag")

	return &Database{
		Providers: providers,
	}, newETag, nil
}

func loadFromCache(cacheFile string) (*CachedData, error) {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache file: %w", err)
	}

	var cached CachedData
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, fmt.Errorf("failed to decode cached data: %w", err)
	}

	return &cached, nil
}

func saveToCache(cacheFile string, database *Database, etag string) error {
	cached := CachedData{
		Database:    *database,
		LastRefresh: time.Now(),
		ETag:        etag,
	}

	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cached data: %w", err)
	}

	if err := os.WriteFile(cacheFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	return nil
}

// datePattern matches date suffixes like -20251101, -2024-11-20, etc.
var datePattern = regexp.MustCompile(`-\d{4}-?\d{2}-?\d{2}$`)

// ResolveModelAlias resolves a model alias to its pinned version.
// For example, ("anthropic", "claude-sonnet-4-5") might resolve to "claude-sonnet-4-5-20250929".
// If the model is not an alias (already pinned or unknown), the original model name is returned.
// This method uses the models.dev database to find the corresponding pinned version.
func (s *Store) ResolveModelAlias(ctx context.Context, providerID, modelName string) string {
	if providerID == "" || modelName == "" {
		return modelName
	}

	// If the model already has a date suffix, it's already pinned
	if datePattern.MatchString(modelName) {
		return modelName
	}

	provider, err := s.getProvider(ctx, providerID)
	if err != nil {
		return modelName
	}

	// Check if the model exists and is marked as "(latest)"
	model, exists := provider.Models[modelName]
	if !exists || !strings.Contains(model.Name, "(latest)") {
		return modelName
	}

	// Find the pinned version by matching the base display name
	// e.g., "Claude Sonnet 4 (latest)" -> "Claude Sonnet 4"
	baseDisplayName := strings.TrimSuffix(model.Name, " (latest)")

	for pinnedID, pinnedModel := range provider.Models {
		if pinnedID != modelName &&
			!strings.Contains(pinnedModel.Name, "(latest)") &&
			pinnedModel.Name == baseDisplayName &&
			datePattern.MatchString(pinnedID) {
			return pinnedID
		}
	}

	return modelName
}

// bedrockRegionPrefixes contains known regional/inference profile prefixes used in Bedrock model IDs.
// These prefixes should be stripped when looking up models in the database since models.dev
// stores models without regional prefixes. AWS uses these for cross-region inference profiles.
// See: https://docs.aws.amazon.com/bedrock/latest/userguide/cross-region-inference.html
var bedrockRegionPrefixes = map[string]bool{
	"us":     true,
	"eu":     true,
	"apac":   true,
	"global": true,
}
