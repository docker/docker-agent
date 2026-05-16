package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/99designs/keyring"
)

// All OAuth tokens for all MCP servers are stored under a single keyring
// item. On macOS each keychain item carries its own ACL, so storing N
// tokens as N items would prompt the user N times the first time each is
// read (and again whenever the binary's signature changes). Bundling them
// collapses that to a single prompt — and a single "Always Allow"
// decision — for any number of MCP servers.
const (
	keyringServiceName = "docker-agent-oauth"
	bundleKey          = "oauth:tokens"

	// Items written by the previous one-token-per-item layout, migrated
	// into the bundle on first load and then removed.
	legacyTokenPrefix = "oauth:"
	legacyIndexKey    = "oauth:_index"
)

// KeyringTokenStore implements OAuthTokenStore by caching the bundled
// keyring item in memory: the OAuth transport's hot path stays in memory
// after the first hit, and writes always target the same keyring item so
// the user's "Always Allow" decision keeps applying to refreshes and to
// new MCP servers.
type KeyringTokenStore struct {
	ring keyring.Keyring

	mu     sync.Mutex
	cache  map[string]*OAuthToken
	loaded bool
}

func openKeyring() (keyring.Keyring, error) {
	return keyring.Open(keyring.Config{
		ServiceName:                    keyringServiceName,
		KeychainTrustApplication:       true,
		KeychainSynchronizable:         false,
		KeychainAccessibleWhenUnlocked: true,
	})
}

// defaultStore returns the process-wide token store, opening the OS
// keyring lazily on first call. Multiple MCP toolsets share its in-memory
// cache so they don't each trigger a credential prompt on construction.
var defaultStore = sync.OnceValue(func() OAuthTokenStore {
	ring, err := openKeyring()
	if err != nil {
		slog.Warn("OS keyring not available, falling back to in-memory token store", "error", err)
		return NewInMemoryTokenStore()
	}
	return newKeyringTokenStore(ring)
})

// NewKeyringTokenStore returns the process-wide token store backed by the
// OS keyring, falling back to InMemoryTokenStore when no backend is
// available. It always returns the same instance.
func NewKeyringTokenStore() OAuthTokenStore {
	return defaultStore()
}

// newKeyringTokenStore wraps an arbitrary keyring with the bundle-and-cache
// store. Used by tests to inject keyring.NewArrayKeyring().
func newKeyringTokenStore(ring keyring.Keyring) *KeyringTokenStore {
	return &KeyringTokenStore{
		ring:  ring,
		cache: map[string]*OAuthToken{},
	}
}

// load fetches the bundled item from the keyring on first use and caches
// it. Subsequent calls are no-ops, so methods can call load() at the top
// of every operation without re-prompting the user. Failures are logged
// but not propagated — an empty in-memory cache lets the OAuth flow
// re-populate fresh tokens, and marking the cache loaded eagerly keeps a
// denied access from snowballing into a prompt on every call.
//
// Caller must hold s.mu.
func (s *KeyringTokenStore) load() {
	if s.loaded {
		return
	}
	s.loaded = true

	item, err := s.ring.Get(bundleKey)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(item.Data, &s.cache); uerr != nil {
			slog.Warn("OAuth token bundle is corrupt; starting fresh", "error", uerr)
			s.cache = map[string]*OAuthToken{}
		}
	case errors.Is(err, keyring.ErrKeyNotFound):
		// Possibly an upgrade from the old per-token layout. Best-effort
		// migration; failures here are silent so an upgrade is never
		// worse than a fresh install.
		if migrated := s.migrateLegacyLocked(); migrated > 0 {
			slog.Debug("Migrated legacy OAuth tokens", "count", migrated)
			if perr := s.persistLocked(); perr != nil {
				slog.Warn("Failed to persist migrated OAuth tokens", "error", perr)
			}
		}
	default:
		slog.Warn("Failed to load OAuth tokens from keyring; using in-memory cache for this process", "error", err)
	}
}

// migrateLegacyLocked folds tokens written by the old per-resource layout
// into s.cache and deletes the legacy entries. Caller must hold s.mu.
func (s *KeyringTokenStore) migrateLegacyLocked() int {
	keys, err := s.ring.Keys()
	if err != nil {
		return 0
	}

	var migrated int
	for _, key := range keys {
		if key == legacyIndexKey {
			_ = s.ring.Remove(key)
			continue
		}
		if key == bundleKey || !strings.HasPrefix(key, legacyTokenPrefix) {
			continue
		}

		item, err := s.ring.Get(key)
		if err != nil {
			continue
		}
		var token OAuthToken
		if json.Unmarshal(item.Data, &token) != nil {
			continue
		}
		s.cache[strings.TrimPrefix(key, legacyTokenPrefix)] = &token
		_ = s.ring.Remove(key)
		migrated++
	}
	return migrated
}

// persistLocked writes the in-memory bundle back to the keyring.
// Caller must hold s.mu.
func (s *KeyringTokenStore) persistLocked() error {
	data, err := json.Marshal(s.cache) //nolint:gosec // OAuth token bundle is intentionally serialized for keyring storage
	if err != nil {
		return fmt.Errorf("failed to marshal token bundle: %w", err)
	}
	return s.ring.Set(keyring.Item{
		Key:   bundleKey,
		Data:  data,
		Label: "Docker Agent OAuth Tokens",
	})
}

func (s *KeyringTokenStore) GetToken(resourceURL string) (*OAuthToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()

	token, ok := s.cache[resourceURL]
	if !ok {
		return nil, fmt.Errorf("no token found for resource: %s", resourceURL)
	}
	return token, nil
}

func (s *KeyringTokenStore) StoreToken(resourceURL string, token *OAuthToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()

	s.cache[resourceURL] = token
	return s.persistLocked()
}

func (s *KeyringTokenStore) RemoveToken(resourceURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()

	if _, ok := s.cache[resourceURL]; !ok {
		return nil
	}
	delete(s.cache, resourceURL)
	return s.persistLocked()
}

// list returns a snapshot of all stored tokens.
func (s *KeyringTokenStore) list() []OAuthTokenEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()

	entries := make([]OAuthTokenEntry, 0, len(s.cache))
	for url, token := range s.cache {
		entries = append(entries, OAuthTokenEntry{ResourceURL: url, Token: token})
	}
	return entries
}

// OAuthTokenEntry pairs a stored OAuth token with its resource URL.
type OAuthTokenEntry struct {
	ResourceURL string
	Token       *OAuthToken
}

// requireKeyring returns the singleton store cast to *KeyringTokenStore,
// or an error if the OS keyring backend is unavailable.
func requireKeyring() (*KeyringTokenStore, error) {
	if s, ok := defaultStore().(*KeyringTokenStore); ok {
		return s, nil
	}
	return nil, errors.New("OS keyring not available")
}

// ListOAuthTokens returns every OAuth token persisted in the keyring.
func ListOAuthTokens() ([]OAuthTokenEntry, error) {
	s, err := requireKeyring()
	if err != nil {
		return nil, err
	}
	return s.list(), nil
}

// RemoveOAuthToken deletes the token stored for resourceURL.
func RemoveOAuthToken(resourceURL string) error {
	s, err := requireKeyring()
	if err != nil {
		return err
	}
	return s.RemoveToken(resourceURL)
}
