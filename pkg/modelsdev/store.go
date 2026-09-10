package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/docker/docker-agent/pkg/desktop/transport"
)

const (
	// APIHost is the models.dev catalog host. Sandbox callers allowlist
	// it so the in-sandbox agent can reach the catalog through the
	// default-deny network proxy.
	APIHost = "models.dev"
	// ModelsDevAPIURL is derived from APIHost so the two can't drift.
	ModelsDevAPIURL = "https://" + APIHost + "/api.json"
	CacheFileName   = "models_dev.json"
	refreshInterval = 24 * time.Hour
)

// fetcher fetches the models.dev catalog. If etag is non-empty it is sent as
// If-None-Match; a 304 response returns (nil, etag, nil) to indicate no change.
type fetcher func(ctx context.Context, etag string) (*Database, string, error)

// Store manages access to the models.dev data.
// All methods are safe for concurrent use.
//
// The database is loaded on first access via GetDatabase and
// then cached in memory for the lifetime of the Store.
type Store struct {
	cacheFile     string
	knownProvider func(string) bool
	// fetch retrieves the catalog from the network. It defaults to
	// fetchFromAPI and is overridable via WithFetcher so tests can stub the
	// network out without mutating package-level state.
	fetch     fetcher
	refreshMu sync.Mutex
	mu        sync.Mutex
	// db is the authoritative catalog from the full (fetch-eligible) load path.
	db *Database
	// cacheDB is a cache-only snapshot served to fetch-disallowed lookups. It is
	// kept separate from db because it may be empty or stale and must never
	// satisfy a later fetch-eligible lookup for a known provider; memoizing it
	// keeps the hot path from re-reading and re-parsing the catalog file on
	// every custom-provider resolution.
	cacheDB *Database
}

// Opt configures a Store created with NewStore.
type Opt func(*storeOptions)

type storeOptions struct {
	cacheFile     string
	knownProvider func(string) bool
	fetch         fetcher
}

// WithCache overrides the path of the on-disk cache file used by the Store.
// The parent directory will be created if it does not already exist.
func WithCache(path string) Opt {
	return func(o *storeOptions) {
		o.cacheFile = path
	}
}

// WithFetcher overrides the function used to fetch the catalog from the
// network. It is primarily a test seam: callers can stub the network out
// (and observe whether a fetch was attempted) without mutating package-level
// state, which keeps such tests safe to run in parallel.
func WithFetcher(fn fetcher) Opt {
	return func(o *storeOptions) {
		o.fetch = fn
	}
}

// WithKnownProvider restricts which provider names may trigger an outbound
// fetch of the models.dev catalog. The predicate should report whether a
// provider is one models.dev could plausibly contain (a built-in or alias).
//
// Looking up a model for a provider the predicate rejects — typically a
// user-defined custom provider whose config (base_url + token) is already
// self-contained — resolves against the cached catalog only and never reaches
// the network. This keeps custom providers working in internet-restricted
// environments instead of blocking on a doomed GET to models.dev (issue #3165).
//
// When no predicate is set, every provider may trigger a fetch (the default,
// backwards-compatible behaviour).
func WithKnownProvider(fn func(string) bool) Opt {
	return func(o *storeOptions) {
		o.knownProvider = fn
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

	fetch := options.fetch
	if fetch == nil {
		fetch = fetchFromAPI
	}
	return &Store{
		cacheFile:     cacheFile,
		knownProvider: options.knownProvider,
		fetch:         fetch,
	}, nil
}

// NewDatabaseStore creates a Store pre-populated with the given database.
// The returned store serves data entirely from memory and never fetches
// from the network or touches the filesystem, making it suitable for
// tests and any scenario where the provider data is already known.
func NewDatabaseStore(db *Database) *Store {
	return &Store{db: db, fetch: fetchFromAPI}
}

// GetDatabase returns the models.dev database, fetching from cache or API as needed.
func (s *Store) GetDatabase(ctx context.Context) (*Database, error) {
	return s.getDatabase(ctx, true)
}

// getDatabase returns the models.dev database. When allowFetch is false the
// catalog is served from memory or the on-disk cache only and the network is
// never touched; a cache miss yields an empty database rather than an error.
func (s *Store) getDatabase(ctx context.Context, allowFetch bool) (*Database, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The authoritative catalog, once loaded, serves every lookup — including
	// fetch-disallowed ones, which then benefit from the fresher data.
	if s.db != nil {
		return s.db, nil
	}

	if !allowFetch {
		// Serve (and memoize) a cache-only snapshot without touching the
		// network. Kept out of s.db so it can never satisfy a later
		// fetch-eligible lookup for a known provider.
		if s.cacheDB == nil {
			s.cacheDB, _ = loadDatabase(ctx, s.cacheFile, false, s.fetcher())
		}
		return s.cacheDB, nil
	}

	db, authoritative := loadDatabase(ctx, s.cacheFile, true, s.fetcher())
	// Only memoize a result that came from the on-disk cache or a live fetch.
	// The embedded fallback snapshot is deliberately NOT pinned, so a later
	// lookup retries the fetch once the network (or cache) recovers instead of
	// serving the build-time snapshot for the Store's entire lifetime.
	if authoritative {
		s.db = db
	}
	return db, nil
}

// Refresh forces a catalog fetch, bypassing the refresh interval. The ETag
// of the on-disk cache is still sent so an unchanged catalog costs only a
// cheap 304 round-trip. On success the catalog replaces the in-memory copies
// and the on-disk cache. A memory-only store (no cache file, e.g. one built
// with NewDatabaseStore) keeps serving its pre-populated database.
func (s *Store) Refresh(ctx context.Context) error {
	if s.cacheFile == "" {
		return errors.New("models.dev catalog refresh is unavailable for an in-memory store")
	}

	// Serialize refreshes without blocking catalog readers during network I/O.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	var etag string
	cached, err := loadFromCache(s.cacheFile)
	if err == nil {
		etag = cached.ETag
	}

	db, newETag, err := s.fetcher()(ctx, etag)
	if err != nil {
		return fmt.Errorf("failed to refresh models.dev catalog: %w", err)
	}

	// db is nil when the server returned 304 Not Modified: the cached catalog
	// is already current, so only its LastRefresh timestamp needs bumping.
	if db == nil {
		if cached == nil {
			return errors.New("models.dev returned not modified without a cached catalog")
		}
		db = &cached.Database
		newETag = cached.ETag
	}

	// Keep file replacement and in-memory installation atomic with respect to
	// normal store loads, but never hold this lock during the network request.
	s.mu.Lock()
	defer s.mu.Unlock()
	if saveErr := saveToCache(s.cacheFile, db, newETag); saveErr != nil {
		slog.WarnContext(ctx, "Failed to save refreshed catalog to cache", "error", saveErr)
	}
	s.db = db
	s.cacheDB = nil
	return nil
}

// fetcher returns the Store's catalog fetcher, defaulting to fetchFromAPI when
// unset. The constructors always populate s.fetch, but defaulting here guards
// against a zero-value or partially-constructed Store panicking on a nil call.
func (s *Store) fetcher() fetcher {
	if s.fetch != nil {
		return s.fetch
	}
	return fetchFromAPI
}

// getProvider returns a specific provider by ID. A provider the Store's
// knownProvider predicate rejects (a user-defined custom provider) is looked
// up without ever fetching the catalog from the network, so a self-contained
// custom-provider config keeps working when models.dev is unreachable.
func (s *Store) getProvider(ctx context.Context, providerID string) (*Provider, error) {
	allowFetch := s.knownProvider == nil || s.knownProvider(providerID)

	db, err := s.getDatabase(ctx, allowFetch)
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
//
// When allowFetch is false the network is never touched: a fresh cache is
// returned as-is, a stale cache is returned regardless of age, and a missing
// cache falls back to the snapshot embedded at build time. This lets lookups
// for providers models.dev cannot know about resolve locally without a doomed
// outbound call, while still offering a real catalog on a cold cache.
//
// loadDatabase always returns a non-nil database. The second return value is
// true when the database came from the on-disk cache or a live fetch, and
// false when it fell back to the snapshot embedded at build time. Callers use
// this to avoid memoizing the fallback so a later lookup can retry once the
// cache or network recovers.
func loadDatabase(ctx context.Context, cacheFile string, allowFetch bool, fetch fetcher) (db *Database, authoritative bool) {
	// Try to load from cache first
	cached, err := loadFromCache(cacheFile)
	if err == nil && (!allowFetch || time.Since(cached.LastRefresh) < refreshInterval) {
		return &cached.Database, true
	}

	if !allowFetch {
		// No fresh cache and fetching is disallowed: use a stale cache if we
		// have one, otherwise fall back to the snapshot baked into the binary
		// so the caller still resolves against a real catalog with no network
		// call.
		if cached != nil {
			return &cached.Database, true
		}
		return embeddedSnapshot(), false
	}

	// Cache is stale or doesn't exist — try a conditional fetch with the ETag.
	var etag string
	if cached != nil {
		etag = cached.ETag
	}

	database, newETag, fetchErr := fetch(ctx, etag)
	if fetchErr != nil {
		// If API fetch fails but we have cached data, use it regardless of age.
		if cached != nil {
			slog.DebugContext(ctx, "API fetch failed, using stale cache", "error", fetchErr)
			return &cached.Database, true
		}
		// No cache either — fall back to the snapshot embedded at build time
		// instead of failing outright, so a fresh binary on a machine that
		// can't reach models.dev still has a usable catalog.
		slog.DebugContext(ctx, "API fetch failed and no cache available, using embedded snapshot", "error", fetchErr)
		return embeddedSnapshot(), false
	}

	// database is nil when the server returned 304 Not Modified.
	if database == nil && cached != nil {
		// Bump LastRefresh so we don't re-check until the next interval.
		cached.LastRefresh = time.Now()
		if saveErr := saveToCache(cacheFile, &cached.Database, cached.ETag); saveErr != nil {
			slog.WarnContext(ctx, "Failed to update cache timestamp", "error", saveErr)
		}
		return &cached.Database, true
	}

	// Save the fresh data to cache.
	if saveErr := saveToCache(cacheFile, database, newETag); saveErr != nil {
		slog.WarnContext(ctx, "Failed to save to cache", "error", saveErr)
	}

	return database, true
}

// Fetch retrieves the models.dev catalog directly from the live API,
// bypassing any on-disk cache or the embedded build-time snapshot. Unlike a
// Store lookup, a failure here is always returned to the caller rather than
// silently degraded to cached or embedded data — callers that need to detect
// live-catalog drift (as opposed to resolving a model as best-effort) should
// use this instead of GetDatabase.
func Fetch(ctx context.Context) (*Database, error) {
	db, _, err := fetchFromAPI(ctx, "")
	if err != nil {
		return nil, err
	}
	if db == nil {
		// fetchFromAPI returns (nil, etag, nil) only for a 304 response, which
		// requires a non-empty If-None-Match — unreachable with the empty etag
		// passed above. Guard it anyway so a future change to fetchFromAPI
		// can't silently turn this into a nil-database success.
		return nil, errors.New("models.dev fetch returned no data")
	}
	return db, nil
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

	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: transport.New(ctx)}).Do(req)
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
