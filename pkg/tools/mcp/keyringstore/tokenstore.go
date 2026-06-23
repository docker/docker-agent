package keyringstore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/99designs/keyring"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

// OAuth tokens are encrypted with AES-256-GCM and written to a single file
// on disk; the encryption key is the only thing kept in the OS keyring.
//
// This split solves two problems that the previous "everything in one
// keyring item" layout could not solve at once:
//
//   - Size: keyring backends cap item size (Windows Credential Manager at
//     2560 bytes, the Linux kernel keyring at a per-user quota). A bundle
//     of several multi-kilobyte OAuth tokens easily blows past that. The
//     key we store is a fixed 32 bytes, so it always fits; the unbounded
//     token blob lives in a file instead.
//   - Prompts: on macOS each keychain item carries its own ACL, so N items
//     means N permission prompts (and re-prompts whenever the binary's
//     signature changes). Keeping exactly one keyring item collapses that
//     to a single prompt — and a single "Always Allow" — no matter how
//     many MCP servers the user authorizes.
const (
	keyringServiceName = "docker-agent-oauth"

	// encryptionKeyItem is the single keyring item we read/write: the
	// AES-256 key that seals the on-disk token file.
	encryptionKeyItem = "oauth:encryption-key"
	encryptionKeySize = 32 // AES-256

	// tokenFileName is the encrypted token bundle, stored under the config
	// dir alongside other docker-agent state.
	tokenFileName = "mcp-oauth-tokens.enc"

	// Legacy keyring keys from the previous "bundle in the keyring"
	// layout. Migrated into the encrypted file on first load, then removed.
	legacyBundleKey   = "oauth:tokens"
	legacyTokenPrefix = "oauth:"
	legacyIndexKey    = "oauth:_index"
)

// KeyringTokenStore keeps OAuth tokens in memory, sealing them to disk with
// a key held in the OS keyring. The keyring is touched at most once per
// process (to fetch or create the key), so the user's "Always Allow"
// decision keeps applying to refreshes and to newly authorized servers.
type KeyringTokenStore struct {
	ring     keyring.Keyring
	filePath string
	write    func(string, []byte) error

	mu      sync.Mutex
	cache   map[string]*mcp.OAuthToken
	key     []byte // cached encryption key; fetched from the keyring once
	loaded  bool
	loadErr error
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
//
// Under `go test` we always return an in-memory store: any test that
// constructs a real *mcp.Toolset (directly or via the mcpcatalog
// builtin) would otherwise reach into the real OS keychain on the first
// outbound HTTP request, popping a macOS password prompt for the
// `docker-agent-oauth` keychain item on developer machines that have a
// token from a prior login.
var defaultStore = sync.OnceValue(func() mcp.OAuthTokenStore {
	if testing.Testing() {
		return mcp.NewInMemoryTokenStore()
	}
	ring, err := openKeyring()
	if err != nil {
		slog.Warn("OS keyring not available, falling back to in-memory token store", "error", err)
		return mcp.NewInMemoryTokenStore()
	}
	return newKeyringTokenStore(ring, filepath.Join(paths.GetConfigDir(), tokenFileName))
})

// Register installs the keyring-backed token store as the default for MCP
// OAuth. The CLI calls this during startup; embedders that don't need
// persistent keyring-backed storage can simply not call it and keep the
// in-memory default.
func Register() {
	mcp.SetDefaultTokenStoreFactory(NewKeyringTokenStore)
}

// NewKeyringTokenStore returns the process-wide token store backed by the
// OS keyring, falling back to InMemoryTokenStore when no backend is
// available. It always returns the same instance.
func NewKeyringTokenStore() mcp.OAuthTokenStore {
	return defaultStore()
}

// newKeyringTokenStore wraps a keyring and an on-disk file path with the
// encrypt-and-cache store. Used by tests to inject keyring.NewArrayKeyring()
// and a temp file.
func newKeyringTokenStore(ring keyring.Keyring, filePath string) *KeyringTokenStore {
	return &KeyringTokenStore{
		ring:     ring,
		filePath: filePath,
		write:    writeTokenFile,
		cache:    map[string]*mcp.OAuthToken{},
	}
}

// load decrypts the on-disk token file on first use and caches it.
// Subsequent calls are no-ops, so methods can call load() at the top of
// every operation without re-touching the keyring. Failures are logged but
// not propagated — an empty in-memory cache lets the OAuth flow re-populate
// fresh tokens, and marking the cache loaded eagerly keeps a denied keyring
// access from snowballing into a prompt on every call.
//
// Caller must hold s.mu.
func (s *KeyringTokenStore) load() {
	if s.loaded {
		return
	}
	s.loaded = true

	data, err := os.ReadFile(s.filePath)
	switch {
	case err == nil:
		key, kerr := s.encryptionKey()
		if kerr != nil {
			slog.Warn("Failed to obtain OAuth encryption key; using in-memory cache for this process", "error", kerr)
			return
		}
		if derr := s.decryptInto(key, data); derr != nil {
			slog.Warn("OAuth token file is corrupt; starting fresh", "error", derr)
			s.cache = map[string]*mcp.OAuthToken{}
		}
	case errors.Is(err, os.ErrNotExist):
		// Possibly an upgrade from an older keyring-only layout. Best-effort
		// migration; failures here are silent so an upgrade is never worse
		// than a fresh install.
		s.migrateLegacyLocked()
	default:
		s.loadErr = fmt.Errorf("failed to read OAuth token file: %w", err)
		slog.Warn("Failed to read OAuth token file; refusing to overwrite it", "error", err)
	}
}

// encryptionKey fetches the AES key from the keyring, generating and
// persisting a fresh one the first time. The key is cached in memory after
// the first call, so the keyring is touched at most once per process.
//
// Caller must hold s.mu.
func (s *KeyringTokenStore) encryptionKey() ([]byte, error) {
	if s.key != nil {
		return s.key, nil
	}

	item, err := s.ring.Get(encryptionKeyItem)
	switch {
	case err == nil:
		if len(item.Data) != encryptionKeySize {
			return nil, fmt.Errorf("stored encryption key has wrong size %d", len(item.Data))
		}
		s.key = item.Data
		return s.key, nil
	case errors.Is(err, keyring.ErrKeyNotFound):
		key := make([]byte, encryptionKeySize)
		if _, rerr := rand.Read(key); rerr != nil {
			return nil, fmt.Errorf("failed to generate encryption key: %w", rerr)
		}
		if serr := s.ring.Set(keyring.Item{
			Key:   encryptionKeyItem,
			Data:  key,
			Label: "Docker Agent OAuth Encryption Key",
		}); serr != nil {
			return nil, fmt.Errorf("failed to store encryption key: %w", serr)
		}
		s.key = key
		return s.key, nil
	default:
		return nil, err
	}
}

// decryptInto decrypts data into s.cache. The nonce is prepended to the
// ciphertext. Caller must hold s.mu.
func (s *KeyringTokenStore) decryptInto(key, data []byte) error {
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	if len(data) < gcm.NonceSize() {
		return errors.New("token file too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return fmt.Errorf("failed to decrypt token file: %w", err)
	}
	cache := map[string]*mcp.OAuthToken{}
	if err := json.Unmarshal(plaintext, &cache); err != nil {
		return fmt.Errorf("failed to unmarshal token bundle: %w", err)
	}
	s.cache = cache
	return nil
}

// migrateLegacyLocked folds tokens written by the previous keyring-only
// layouts (the single "oauth:tokens" bundle, or the even older
// per-resource items) into s.cache, persists them to the encrypted file,
// and only then removes the legacy keyring entries.
//
// The persist-before-delete ordering is deliberate: if the file write
// fails we leave the legacy entries untouched so the next process can
// retry, rather than deleting the only copy of the user's tokens. The
// in-memory cache is still populated so the current session keeps working.
//
// Caller must hold s.mu.
func (s *KeyringTokenStore) migrateLegacyLocked() {
	unlock, err := lockTokenFile(s.filePath)
	if err != nil {
		slog.Warn("Failed to lock OAuth token file for migration; leaving legacy keyring entries in place for retry", "error", err)
		return
	}
	defer unlock()

	if err := s.reloadFromDiskLocked(); err != nil {
		slog.Warn("Failed to reload OAuth token file during migration; leaving legacy keyring entries in place for retry", "error", err)
		return
	}

	legacyKeys := s.collectLegacyTokensLocked()
	if len(legacyKeys) == 0 {
		return
	}

	if err := s.persistLocked(); err != nil {
		slog.Warn("Failed to persist migrated OAuth tokens; leaving legacy keyring entries in place for retry", "error", err)
		return
	}
	slog.Debug("Migrated legacy OAuth tokens", "count", len(s.cache))

	for _, key := range legacyKeys {
		_ = s.ring.Remove(key)
	}
}

// collectLegacyTokensLocked reads tokens from both legacy keyring layouts
// into s.cache and returns the keyring keys that should be removed once the
// migration has been safely persisted. It does not mutate the keyring.
//
// Caller must hold s.mu.
func (s *KeyringTokenStore) collectLegacyTokensLocked() []string {
	var legacyKeys []string
	legacyTokens := map[string]*mcp.OAuthToken{}

	// Oldest legacy layout: one keyring item per resource URL.
	keys, err := s.ring.Keys()
	if err == nil {
		for _, key := range keys {
			if key == legacyIndexKey {
				legacyKeys = append(legacyKeys, key)
				continue
			}
			if key == legacyBundleKey || key == encryptionKeyItem || !strings.HasPrefix(key, legacyTokenPrefix) {
				continue
			}

			item, err := s.ring.Get(key)
			if err != nil {
				continue
			}
			var token mcp.OAuthToken
			if json.Unmarshal(item.Data, &token) != nil {
				continue
			}
			legacyTokens[strings.TrimPrefix(key, legacyTokenPrefix)] = &token
			legacyKeys = append(legacyKeys, key)
		}
	}

	// Newer legacy layout: a single JSON bundle under one keyring item. It
	// intentionally wins over per-resource items for the same URL.
	if item, err := s.ring.Get(legacyBundleKey); err == nil {
		bundle := map[string]*mcp.OAuthToken{}
		if json.Unmarshal(item.Data, &bundle) == nil {
			maps.Copy(legacyTokens, bundle)
		}
		legacyKeys = append(legacyKeys, legacyBundleKey)
	}

	// The encrypted file is the newest source of truth. Legacy tokens only
	// fill gaps, which prevents a stale legacy entry from overwriting a token
	// another process just persisted while this process was starting.
	for url, token := range legacyTokens {
		if _, exists := s.cache[url]; !exists {
			s.cache[url] = token
		}
	}
	return legacyKeys
}

// persistLocked encrypts the in-memory bundle and writes it atomically to
// disk. Caller must hold s.mu.
func (s *KeyringTokenStore) persistLocked() error {
	key, err := s.encryptionKey()
	if err != nil {
		return err
	}

	plaintext, err := json.Marshal(s.cache) //nolint:gosec // OAuth token bundle is intentionally serialized for storage
	if err != nil {
		return fmt.Errorf("failed to marshal token bundle: %w", err)
	}

	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)

	if err := ensurePrivateDir(filepath.Dir(s.filePath)); err != nil {
		return err
	}
	return s.write(s.filePath, sealed)
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create token directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs execute bit; 0700 is owner-only
		return fmt.Errorf("failed to secure token directory: %w", err)
	}
	return nil
}

// reloadFromDiskLocked refreshes s.cache from the encrypted file while the
// caller holds both s.mu and the cross-process file lock. A missing file is
// not an error; a corrupt file is treated as empty so StoreToken can recover
// by writing a fresh encrypted bundle.
func (s *KeyringTokenStore) reloadFromDiskLocked() error {
	data, err := os.ReadFile(s.filePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("failed to read OAuth token file: %w", err)
	}
	key, err := s.encryptionKey()
	if err != nil {
		return err
	}
	if err := s.decryptInto(key, data); err != nil {
		slog.Warn("OAuth token file is corrupt; overwriting with fresh token bundle", "error", err)
		s.cache = map[string]*mcp.OAuthToken{}
	}
	return nil
}

func writeTokenFile(path string, data []byte) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicfile.Write(path, bytes.NewReader(data), 0o600)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}
	return gcm, nil
}

func (s *KeyringTokenStore) GetToken(resourceURL string) (*mcp.OAuthToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()

	token, ok := s.cache[resourceURL]
	if !ok {
		return nil, fmt.Errorf("no token found for resource: %s", resourceURL)
	}
	return token, nil
}

func (s *KeyringTokenStore) StoreToken(resourceURL string, token *mcp.OAuthToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if s.loadErr != nil {
		return s.loadErr
	}

	unlock, err := lockTokenFile(s.filePath)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reloadFromDiskLocked(); err != nil {
		return err
	}

	s.cache[resourceURL] = token
	return s.persistLocked()
}

func (s *KeyringTokenStore) RemoveToken(resourceURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if s.loadErr != nil {
		return s.loadErr
	}

	unlock, err := lockTokenFile(s.filePath)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reloadFromDiskLocked(); err != nil {
		return err
	}

	if _, ok := s.cache[resourceURL]; !ok {
		return nil
	}
	delete(s.cache, resourceURL)
	return s.persistLocked()
}

// ListOAuthTokens returns a snapshot of all stored tokens.
func (s *KeyringTokenStore) ListOAuthTokens() []mcp.OAuthTokenEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()

	entries := make([]mcp.OAuthTokenEntry, 0, len(s.cache))
	for url, token := range s.cache {
		entries = append(entries, mcp.OAuthTokenEntry{ResourceURL: url, Token: token})
	}
	return entries
}
