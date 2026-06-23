package keyringstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/99designs/keyring"

	"github.com/docker/docker-agent/pkg/tools/mcp"
)

// newTestStore returns a KeyringTokenStore backed by an in-memory array
// keyring and a token file inside a fresh temp dir.
func newTestStore(t *testing.T) (*KeyringTokenStore, keyring.Keyring) {
	t.Helper()
	ring := keyring.NewArrayKeyring(nil)
	path := filepath.Join(t.TempDir(), tokenFileName)
	return newKeyringTokenStore(ring, path), ring
}

func TestKeyringTokenStore_RoundTrip(t *testing.T) {
	// Use in-memory store to avoid triggering macOS keychain permission dialogs
	// or failing in CI environments without a keyring.
	store := mcp.NewInMemoryTokenStore()

	resourceURL := "https://example.com/mcp"

	// Initially no token
	if _, err := store.GetToken(resourceURL); err == nil {
		t.Fatal("expected error for missing token")
	}

	// Store a token
	token := &mcp.OAuthToken{
		AccessToken:  "access-123",
		TokenType:    "Bearer",
		RefreshToken: "refresh-456",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	if err := store.StoreToken(resourceURL, token); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}

	// Retrieve it
	got, err := store.GetToken(resourceURL)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got.AccessToken != "access-123" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "access-123")
	}
	if got.RefreshToken != "refresh-456" {
		t.Errorf("RefreshToken = %q, want %q", got.RefreshToken, "refresh-456")
	}

	// Remove it
	if err := store.RemoveToken(resourceURL); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}
	if _, err := store.GetToken(resourceURL); err == nil {
		t.Fatal("expected error after RemoveToken")
	}
}

func TestKeyringTokenStore_JSONRoundTrip(t *testing.T) {
	// Verify that mcp.OAuthToken serializes correctly (important for keyring storage)
	token := &mcp.OAuthToken{
		AccessToken:  "at",
		TokenType:    "Bearer",
		RefreshToken: "rt",
		ExpiresIn:    7200,
		ExpiresAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Scope:        "read write",
	}

	data, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got mcp.OAuthToken
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.AccessToken != token.AccessToken || got.RefreshToken != token.RefreshToken || got.Scope != token.Scope {
		t.Errorf("JSON round-trip mismatch: got %+v, want %+v", got, token)
	}
}

func TestKeyringTokenStore_RemoveNonExistent(t *testing.T) {
	store := mcp.NewInMemoryTokenStore()
	if err := store.RemoveToken("https://nonexistent.example.com"); err != nil {
		t.Fatalf("RemoveToken for non-existent key should not error: %v", err)
	}
}

// TestEncryptedStore_PersistsAcrossReload verifies tokens written by one
// store instance are readable by a fresh instance pointed at the same
// keyring + file — i.e. they survive a process restart.
func TestEncryptedStore_PersistsAcrossReload(t *testing.T) {
	store, ring := newTestStore(t)

	urls := []string{
		"https://server-a.example/mcp",
		"https://server-b.example/mcp",
		"https://server-c.example/mcp",
	}
	for i, url := range urls {
		if err := store.StoreToken(url, &mcp.OAuthToken{AccessToken: "at-" + string(rune('A'+i))}); err != nil {
			t.Fatalf("StoreToken(%s): %v", url, err)
		}
	}

	// A fresh store over the same ring + file (like a new process) must
	// decrypt and surface every token.
	fresh := newKeyringTokenStore(ring, store.filePath)
	for i, url := range urls {
		got, err := fresh.GetToken(url)
		if err != nil {
			t.Fatalf("GetToken(%s): %v", url, err)
		}
		if want := "at-" + string(rune('A'+i)); got.AccessToken != want {
			t.Errorf("AccessToken for %s = %q, want %q", url, got.AccessToken, want)
		}
	}
}

// countingKeyring tracks how many times Get and Set are invoked on the
// underlying ring. Used to assert that the store touches the keyring at
// most once per process regardless of how many tokens are looked up — the
// property that avoids the "many keychain prompts on macOS" problem.
type countingKeyring struct {
	keyring.Keyring

	gets, sets int
}

func newCountingKeyring() *countingKeyring {
	return &countingKeyring{Keyring: keyring.NewArrayKeyring(nil)}
}

func (k *countingKeyring) Get(key string) (keyring.Item, error) {
	k.gets++
	return k.Keyring.Get(key)
}

func (k *countingKeyring) Set(item keyring.Item) error {
	k.sets++
	return k.Keyring.Set(item)
}

// TestEncryptedStore_ReadsTouchKeyringOnce verifies that, regardless of how
// many resource URLs are looked up, a process touches the keyring exactly
// once (to fetch the encryption key). Everything else is served from the
// in-memory cache or the encrypted file. This is what avoids repeated
// keychain prompts on macOS.
func TestEncryptedStore_ReadsTouchKeyringOnce(t *testing.T) {
	urls := []string{
		"https://server-a.example/mcp",
		"https://server-b.example/mcp",
		"https://server-c.example/mcp",
	}

	ring := newCountingKeyring()
	path := filepath.Join(t.TempDir(), tokenFileName)
	store := newKeyringTokenStore(ring, path)
	for i, url := range urls {
		if err := store.StoreToken(url, &mcp.OAuthToken{AccessToken: "at-" + string(rune('A'+i))}); err != nil {
			t.Fatalf("StoreToken(%s): %v", url, err)
		}
	}

	// Drop the cache by wrapping the same ring + file with a fresh store,
	// so we exercise a real load() like a new process would.
	ring.gets, ring.sets = 0, 0
	fresh := newKeyringTokenStore(ring, path)

	// Read each token several times; only the first read should hit the
	// keyring (for the encryption key).
	for range 5 {
		for _, url := range urls {
			if _, err := fresh.GetToken(url); err != nil {
				t.Fatalf("GetToken(%s): %v", url, err)
			}
		}
	}

	if ring.gets != 1 {
		t.Errorf("expected exactly 1 underlying keyring Get (encryption key), got %d", ring.gets)
	}
	if ring.sets != 0 {
		t.Errorf("read-only path must not write to the keyring, got %d Set calls", ring.sets)
	}
}

// TestEncryptedStore_WritesTouchKeyringOnce verifies repeated writes within
// a process reuse the cached encryption key rather than re-fetching it from
// the keyring on every persist.
func TestEncryptedStore_WritesTouchKeyringOnce(t *testing.T) {
	ring := newCountingKeyring()
	path := filepath.Join(t.TempDir(), tokenFileName)
	store := newKeyringTokenStore(ring, path)

	for i := range 5 {
		url := fmt.Sprintf("https://server-%d.example/mcp", i)
		if err := store.StoreToken(url, &mcp.OAuthToken{AccessToken: "at"}); err != nil {
			t.Fatalf("StoreToken(%s): %v", url, err)
		}
	}
	if err := store.RemoveToken("https://server-0.example/mcp"); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}

	// The encryption key is fetched and created once on the first
	// operation; every subsequent write must reuse the cached key and not
	// touch the encryption-key item again. (The one-time Set is the key
	// creation.)
	if ring.sets != 1 {
		t.Errorf("expected exactly 1 keyring Set (key creation) across many writes, got %d", ring.sets)
	}
}

func TestEncryptedStore_GetMissDoesNotCreateKeyringItem(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	store := newKeyringTokenStore(ring, filepath.Join(t.TempDir(), tokenFileName))

	if _, err := store.GetToken("https://missing.example/mcp"); err == nil {
		t.Fatal("expected missing token error")
	}

	keys, err := ring.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("missing-token lookup should not create keyring entries, got %v", keys)
	}
}

func TestEncryptedStore_CrossProcessStoresMerge(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	path := filepath.Join(t.TempDir(), tokenFileName)

	processA := newKeyringTokenStore(ring, path)
	processB := newKeyringTokenStore(ring, path)

	if err := processA.StoreToken("https://a.example/mcp", &mcp.OAuthToken{AccessToken: "a"}); err != nil {
		t.Fatalf("processA initial StoreToken: %v", err)
	}
	// Process B loads the same initial state before A writes again.
	if _, err := processB.GetToken("https://a.example/mcp"); err != nil {
		t.Fatalf("processB load: %v", err)
	}
	if err := processA.StoreToken("https://c.example/mcp", &mcp.OAuthToken{AccessToken: "c"}); err != nil {
		t.Fatalf("processA second StoreToken: %v", err)
	}
	if err := processB.StoreToken("https://b.example/mcp", &mcp.OAuthToken{AccessToken: "b"}); err != nil {
		t.Fatalf("processB StoreToken: %v", err)
	}

	fresh := newKeyringTokenStore(ring, path)
	for url, want := range map[string]string{
		"https://a.example/mcp": "a",
		"https://b.example/mcp": "b",
		"https://c.example/mcp": "c",
	} {
		got, err := fresh.GetToken(url)
		if err != nil {
			t.Fatalf("GetToken(%s): %v", url, err)
		}
		if got.AccessToken != want {
			t.Errorf("AccessToken for %s = %q, want %q", url, got.AccessToken, want)
		}
	}
}

// TestEncryptedStore_KeyringHoldsOnlyTheKey verifies that no matter how
// many resource URLs are stored, the keyring holds exactly one item: the
// encryption key. The tokens themselves live in the file, so macOS only
// ever asks for permission on a single ACL.
func TestEncryptedStore_KeyringHoldsOnlyTheKey(t *testing.T) {
	store, ring := newTestStore(t)

	for _, url := range []string{
		"https://server-a.example/mcp",
		"https://server-b.example/mcp",
		"https://server-c.example/mcp",
	} {
		if err := store.StoreToken(url, &mcp.OAuthToken{AccessToken: "at"}); err != nil {
			t.Fatalf("StoreToken(%s): %v", url, err)
		}
	}

	keys, err := ring.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != encryptionKeyItem {
		t.Fatalf("expected single keyring item %q, got %v", encryptionKeyItem, keys)
	}
}

// TestEncryptedStore_FileIsNotPlaintext guards against accidentally writing
// tokens in the clear: the access token must not be findable in the file.
func TestEncryptedStore_FileIsNotPlaintext(t *testing.T) {
	store, _ := newTestStore(t)

	const secret = "super-secret-access-token-value"
	if err := store.StoreToken("https://a.example/mcp", &mcp.OAuthToken{AccessToken: secret}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}

	data, err := os.ReadFile(store.filePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("token file is empty")
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Fatal("access token found in plaintext in the token file")
	}
}

// TestEncryptedStore_FilePermissions checks the token file is created with
// owner-only permissions.
func TestEncryptedStore_FilePermissions(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.StoreToken("https://a.example/mcp", &mcp.OAuthToken{AccessToken: "x"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}
	info, err := os.Stat(store.filePath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file permissions = %o, want 0600", perm)
	}
}

// TestEncryptedStore_LegacyBundleMigration confirms tokens previously
// stored in the single keyring bundle are folded into the encrypted file
// on first load and the legacy keyring entry is cleaned up.
func TestEncryptedStore_LegacyBundleMigration(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	path := filepath.Join(t.TempDir(), tokenFileName)

	bundle := map[string]*mcp.OAuthToken{
		"https://legacy-a.example/mcp": {AccessToken: "legacy-a"},
		"https://legacy-b.example/mcp": {AccessToken: "legacy-b"},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal legacy bundle: %v", err)
	}
	if err := ring.Set(keyring.Item{Key: legacyBundleKey, Data: data}); err != nil {
		t.Fatalf("seed legacy bundle: %v", err)
	}

	store := newKeyringTokenStore(ring, path)
	for url, want := range bundle {
		got, err := store.GetToken(url)
		if err != nil {
			t.Fatalf("GetToken(%s): %v", url, err)
		}
		if got.AccessToken != want.AccessToken {
			t.Errorf("AccessToken for %s = %q, want %q", url, got.AccessToken, want.AccessToken)
		}
	}

	// The legacy bundle key must be gone; only the encryption key remains.
	keys, err := ring.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != encryptionKeyItem {
		t.Errorf("expected only encryption key after migration, got %v", keys)
	}

	// Tokens must now survive a reload purely from the file.
	fresh := newKeyringTokenStore(ring, path)
	if _, err := fresh.GetToken("https://legacy-a.example/mcp"); err != nil {
		t.Errorf("migrated token not readable after reload: %v", err)
	}
}

// TestEncryptedStore_LegacyPerTokenMigration confirms the oldest layout —
// one keyring item per resource URL — is migrated and cleaned up.
func TestEncryptedStore_LegacyPerTokenMigration(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	path := filepath.Join(t.TempDir(), tokenFileName)

	urls := []string{
		"https://legacy-a.example/mcp",
		"https://legacy-b.example/mcp",
	}
	for _, url := range urls {
		seedLegacyToken(t, ring, url, &mcp.OAuthToken{AccessToken: "legacy-" + url})
	}
	// Also seed the legacy index, which should be removed without becoming
	// a token entry.
	if err := ring.Set(keyring.Item{
		Key:  legacyIndexKey,
		Data: []byte(`["https://legacy-a.example/mcp"]`),
	}); err != nil {
		t.Fatalf("seed legacy index: %v", err)
	}

	store := newKeyringTokenStore(ring, path)
	for _, url := range urls {
		got, err := store.GetToken(url)
		if err != nil {
			t.Fatalf("GetToken(%s): %v", url, err)
		}
		if want := "legacy-" + url; got.AccessToken != want {
			t.Errorf("AccessToken for %s = %q, want %q", url, got.AccessToken, want)
		}
	}

	keys, err := ring.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != encryptionKeyItem {
		t.Errorf("expected only encryption key after migration, got %v", keys)
	}
}

func TestEncryptedStore_LegacyBundleWinsOverPerTokenMigration(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	path := filepath.Join(t.TempDir(), tokenFileName)
	const url = "https://legacy.example/mcp"

	seedLegacyToken(t, ring, url, &mcp.OAuthToken{AccessToken: "old-per-token"})
	bundleData, err := json.Marshal(map[string]*mcp.OAuthToken{
		url: {AccessToken: "new-bundle"},
	})
	if err != nil {
		t.Fatalf("marshal legacy bundle: %v", err)
	}
	if err := ring.Set(keyring.Item{Key: legacyBundleKey, Data: bundleData}); err != nil {
		t.Fatalf("seed legacy bundle: %v", err)
	}

	got, err := newKeyringTokenStore(ring, path).GetToken(url)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got.AccessToken != "new-bundle" {
		t.Fatalf("AccessToken = %q, want newer bundle token", got.AccessToken)
	}
}

func seedLegacyToken(t *testing.T, ring keyring.Keyring, url string, tok *mcp.OAuthToken) {
	t.Helper()
	data, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal legacy token: %v", err)
	}
	if err := ring.Set(keyring.Item{Key: legacyTokenPrefix + url, Data: data}); err != nil {
		t.Fatalf("seed legacy item: %v", err)
	}
}

// TestEncryptedStore_MigrationKeepsLegacyOnPersistFailure is the regression
// test for the data-loss window: if the encrypted file cannot be written,
// migration must NOT delete the legacy keyring entries, so a later process
// can retry instead of leaving the user with no tokens at all.
func TestEncryptedStore_MigrationKeepsLegacyOnPersistFailure(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)

	bundle := map[string]*mcp.OAuthToken{
		"https://legacy-a.example/mcp": {AccessToken: "legacy-a"},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal legacy bundle: %v", err)
	}
	if err := ring.Set(keyring.Item{Key: legacyBundleKey, Data: data}); err != nil {
		t.Fatalf("seed legacy bundle: %v", err)
	}

	store := newKeyringTokenStore(ring, filepath.Join(t.TempDir(), tokenFileName))
	store.write = func(string, []byte) error { return errors.New("simulated persist failure") }

	// The token is still served from the in-memory cache for this session.
	got, err := store.GetToken("https://legacy-a.example/mcp")
	if err != nil {
		t.Fatalf("GetToken after failed-persist migration: %v", err)
	}
	if got.AccessToken != "legacy-a" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "legacy-a")
	}

	// The legacy bundle must still be in the keyring so the next process
	// can retry the migration.
	if _, err := ring.Get(legacyBundleKey); err != nil {
		t.Errorf("legacy bundle must survive a failed persist so migration can retry, got: %v", err)
	}
}

func TestEncryptedStore_StoreRefusesAfterUnreadableFile(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	store := newKeyringTokenStore(ring, filepath.Join(blocker, tokenFileName))

	if err := store.StoreToken("https://a.example/mcp", &mcp.OAuthToken{AccessToken: "a"}); err == nil {
		t.Fatal("StoreToken should refuse to overwrite after unreadable token file")
	}
}

// TestEncryptedStore_RemovePersists verifies that deleting a token rewrites
// the file without it, so subsequent reads no longer see it even after the
// in-memory cache is dropped.
func TestEncryptedStore_RemovePersists(t *testing.T) {
	store, ring := newTestStore(t)

	url := "https://to-remove.example/mcp"
	if err := store.StoreToken(url, &mcp.OAuthToken{AccessToken: "x"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}
	if err := store.RemoveToken(url); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}

	if _, err := newKeyringTokenStore(ring, store.filePath).GetToken(url); err == nil {
		t.Fatal("expected GetToken to fail after RemoveToken")
	}
}

// TestEncryptedStore_CorruptFile ensures a corrupt token file doesn't crash
// callers — we treat it as empty and let the OAuth flow re-populate.
func TestEncryptedStore_CorruptFile(t *testing.T) {
	store, ring := newTestStore(t)

	// Seed the encryption key by writing a real token first, then clobber
	// the file with garbage that won't decrypt.
	if err := store.StoreToken("https://seed.example/mcp", &mcp.OAuthToken{AccessToken: "seed"}); err != nil {
		t.Fatalf("seed StoreToken: %v", err)
	}
	if err := os.WriteFile(store.filePath, []byte("not-encrypted-garbage"), 0o600); err != nil {
		t.Fatalf("clobber file: %v", err)
	}

	fresh := newKeyringTokenStore(ring, store.filePath)
	if _, err := fresh.GetToken("https://anything.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token, got nil")
	}

	// StoreToken on top of a corrupt file should overwrite it.
	if err := fresh.StoreToken("https://anything.example/mcp", &mcp.OAuthToken{AccessToken: "fresh"}); err != nil {
		t.Fatalf("StoreToken after corrupt file: %v", err)
	}
	got, err := fresh.GetToken("https://anything.example/mcp")
	if err != nil || got.AccessToken != "fresh" {
		t.Fatalf("expected fresh token after recovery, got token=%v err=%v", got, err)
	}
}

// TestEncryptedStore_ListReturnsAllEntries exercises the list helper used by
// `agent debug oauth list`.
func TestEncryptedStore_ListReturnsAllEntries(t *testing.T) {
	store, ring := newTestStore(t)

	if err := store.StoreToken("https://a.example/mcp", &mcp.OAuthToken{AccessToken: "a"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}
	if err := store.StoreToken("https://b.example/mcp", &mcp.OAuthToken{AccessToken: "b"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}

	// Reload from the ring + file (mirroring what a fresh process would do).
	entries := newKeyringTokenStore(ring, store.filePath).ListOAuthTokens()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	byURL := map[string]string{}
	for _, e := range entries {
		byURL[e.ResourceURL] = e.Token.AccessToken
	}
	if byURL["https://a.example/mcp"] != "a" || byURL["https://b.example/mcp"] != "b" {
		t.Errorf("unexpected entries: %+v", byURL)
	}
}

// failingKeyring returns a fixed error from Get; used to make sure the store
// doesn't permanently re-prompt when keychain access is denied.
type failingKeyring struct {
	keyring.Keyring

	getErr error
}

func (k *failingKeyring) Get(string) (keyring.Item, error) {
	return keyring.Item{}, k.getErr
}

// Other Keyring methods aren't called on this test's path, but provide
// no-op implementations so a stray call wouldn't nil-deref the embedded
// interface.
func (*failingKeyring) Set(keyring.Item) error                       { return nil }
func (*failingKeyring) Remove(string) error                          { return nil }
func (*failingKeyring) Keys() ([]string, error)                      { return nil, nil }
func (*failingKeyring) GetMetadata(string) (keyring.Metadata, error) { return keyring.Metadata{}, nil }

// TestEncryptedStore_KeyringFailureIsCachedOnce checks that a keyring
// failure does not turn into an avalanche of repeated prompts: load() marks
// the cache as loaded eagerly so a denied access surfaces once per process,
// not once per token operation.
func TestEncryptedStore_KeyringFailureIsCachedOnce(t *testing.T) {
	ring := &failingKeyring{getErr: errors.New("simulated denied access")}
	store := newKeyringTokenStore(ring, filepath.Join(t.TempDir(), tokenFileName))

	if _, err := store.GetToken("https://a.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token after denied keyring access")
	}
	if _, err := store.GetToken("https://b.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token after denied keyring access")
	}
}

// TestKeyringTokenStore_ConcurrentAccess verifies that concurrent reads and
// writes to the token store are safe and don't cause data races.
func TestKeyringTokenStore_ConcurrentAccess(t *testing.T) {
	store, _ := newTestStore(t)

	const numGoroutines = 10
	const numOperations = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines * 3) // readers, writers, removers

	// Concurrent readers
	for i := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			url := fmt.Sprintf("https://server-%d.example/mcp", id%3)
			for range numOperations {
				_, _ = store.GetToken(url)
			}
		}(i)
	}

	// Concurrent writers
	for i := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			url := fmt.Sprintf("https://server-%d.example/mcp", id%3)
			for j := range numOperations {
				_ = store.StoreToken(url, &mcp.OAuthToken{
					AccessToken: fmt.Sprintf("token-%d-%d", id, j),
				})
			}
		}(i)
	}

	// Concurrent removers
	for i := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			url := fmt.Sprintf("https://server-%d.example/mcp", id%3)
			for range numOperations {
				_ = store.RemoveToken(url)
			}
		}(i)
	}

	wg.Wait()

	// Verify the store is still in a consistent state
	entries := store.ListOAuthTokens()
	t.Logf("After concurrent access: %d tokens remain", len(entries))
}
