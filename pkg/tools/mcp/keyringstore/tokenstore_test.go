package keyringstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// Register runs on every root.Execute, so a process that already materialized
// the default store (e.g. by building a remote MCP toolset) must be able to
// call it again without tripping the panic in SetDefaultTokenStoreFactory.
func TestRegister_IdempotentAfterStoreCreation(t *testing.T) {
	Register()
	_ = mcp.NewKeyringTokenStore() // materialize the process-wide store
	Register()                     // must be a no-op, not a panic
}

func TestKeyringTokenStore_RoundTrip(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	store := mcp.NewInMemoryTokenStore()
	if err := store.RemoveToken("https://nonexistent.example.com"); err != nil {
		t.Fatalf("RemoveToken for non-existent key should not error: %v", err)
	}
}

// TestEncryptedStore_PersistsAcrossReload verifies tokens written by one
// store instance are readable by a fresh instance pointed at the same
// keyring + file — i.e. they survive a process restart.
func TestEncryptedStore_PersistsAcrossReload(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	store, _ := newTestStore(t)
	if err := store.StoreToken("https://a.example/mcp", &mcp.OAuthToken{AccessToken: "x"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}
	info, err := os.Stat(store.filePath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	expectedPerm := os.FileMode(0o600)
	if runtime.GOOS == "windows" {
		expectedPerm = 0o666
	}
	if perm := info.Mode().Perm(); perm != expectedPerm {
		t.Errorf("token file permissions = %o, want %o", perm, expectedPerm)
	}
}

// TestEncryptedStore_LegacyBundleMigration confirms tokens previously
// stored in the single keyring bundle are folded into the encrypted file
// on first load and the legacy keyring entry is cleaned up.
func TestEncryptedStore_LegacyBundleMigration(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	ring := &failingKeyring{getErr: errors.New("simulated denied access")}
	store := newKeyringTokenStore(ring, filepath.Join(t.TempDir(), tokenFileName))

	if _, err := store.GetToken("https://a.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token after denied keyring access")
	}
	if _, err := store.GetToken("https://b.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token after denied keyring access")
	}
}

// TestSecureBackendsExcludeGenericFile guards the fix for the sandbox failure:
// the generic "file" and "pass" backends must never be in the secure list, or
// openKeyring would silently return a half-broken backend instead of an error
// the caller can fall back from.
func TestSecureBackendsExcludeGenericFile(t *testing.T) {
	t.Parallel()
	for _, b := range secureBackends {
		if b == keyring.FileBackend || b == keyring.PassBackend {
			t.Fatalf("secureBackends must not contain the generic %q backend", b)
		}
	}
}

// TestOpenFileKeyring_Persists verifies the file-backed fallback used inside a
// sandbox actually persists an item to disk and survives a reopen, so tokens
// don't vanish between runs.
func TestOpenFileKeyring_Persists(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), fallbackKeyringDir)

	ring, err := openFileKeyring(dir)
	if err != nil {
		t.Fatalf("openFileKeyring: %v", err)
	}
	if err := ring.Set(keyring.Item{Key: encryptionKeyItem, Data: []byte("secret")}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	reopened, err := openFileKeyring(dir)
	if err != nil {
		t.Fatalf("reopen openFileKeyring: %v", err)
	}
	item, err := reopened.Get(encryptionKeyItem)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(item.Data) != "secret" {
		t.Errorf("Data = %q, want %q", item.Data, "secret")
	}
}

// TestFileKeyringBackedStore_PersistsAcrossReload exercises the full fallback
// path a sandbox would take: a KeyringTokenStore backed by the file keyring
// must round-trip tokens across a simulated process restart.
func TestFileKeyringBackedStore_PersistsAcrossReload(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	keyringDir := filepath.Join(root, fallbackKeyringDir)
	tokenPath := filepath.Join(root, tokenFileName)

	ring, err := openFileKeyring(keyringDir)
	if err != nil {
		t.Fatalf("openFileKeyring: %v", err)
	}

	const url = "https://mcp.notion.com/sse"
	if err := newKeyringTokenStore(ring, tokenPath).StoreToken(url, &mcp.OAuthToken{AccessToken: "notion-token"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}

	reopened, err := openFileKeyring(keyringDir)
	if err != nil {
		t.Fatalf("reopen openFileKeyring: %v", err)
	}
	got, err := newKeyringTokenStore(reopened, tokenPath).GetToken(url)
	if err != nil {
		t.Fatalf("GetToken after reload: %v", err)
	}
	if got.AccessToken != "notion-token" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "notion-token")
	}
}

// TestFileKeyringPassphrase_RandomAndPersistent verifies the file-keyring
// passphrase is generated randomly on first use, persisted with owner-only
// permissions, and reused on subsequent calls — so it is unique per install
// rather than a hardcoded constant baked into the binary.
func TestFileKeyringPassphrase_RandomAndPersistent(t *testing.T) {
	t.Parallel()
	dirA := filepath.Join(t.TempDir(), fallbackKeyringDir)
	if err := ensurePrivateDir(dirA); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}

	first, err := fileKeyringPassphrase(dirA)
	if err != nil {
		t.Fatalf("fileKeyringPassphrase: %v", err)
	}
	if first == "" {
		t.Fatal("passphrase must not be empty")
	}

	// A second call in the same dir must reuse the persisted passphrase.
	second, err := fileKeyringPassphrase(dirA)
	if err != nil {
		t.Fatalf("fileKeyringPassphrase (reuse): %v", err)
	}
	if second != first {
		t.Errorf("passphrase changed across calls: %q != %q", second, first)
	}

	// The passphrase file must be owner-only.
	info, err := os.Stat(filepath.Join(dirA, fallbackPassphraseFile))
	if err != nil {
		t.Fatalf("Stat passphrase file: %v", err)
	}
	expectedPerm := os.FileMode(0o600)
	if runtime.GOOS == "windows" {
		expectedPerm = 0o666
	}
	if perm := info.Mode().Perm(); perm != expectedPerm {
		t.Errorf("passphrase file permissions = %o, want %o", perm, expectedPerm)
	}

	// A different install dir must get a different passphrase.
	dirB := filepath.Join(t.TempDir(), fallbackKeyringDir)
	if err := ensurePrivateDir(dirB); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	other, err := fileKeyringPassphrase(dirB)
	if err != nil {
		t.Fatalf("fileKeyringPassphrase (dirB): %v", err)
	}
	if other == first {
		t.Error("two independent installs must not share a passphrase")
	}
}

// TestBuildDefaultStore_PrefersNativeKeyring verifies the native keyring is
// used when it opens successfully, without ever consulting the fallback.
func TestBuildDefaultStore_PrefersNativeKeyring(t *testing.T) {
	t.Parallel()
	native := keyring.NewArrayKeyring(nil)
	fallbackCalled := false

	store := buildDefaultStore(t.TempDir(),
		func() (keyring.Keyring, error) { return native, nil },
		func(string) (keyring.Keyring, error) {
			fallbackCalled = true
			return nil, errors.New("should not be called")
		},
	)

	if _, ok := store.(*KeyringTokenStore); !ok {
		t.Fatalf("expected *KeyringTokenStore, got %T", store)
	}
	if fallbackCalled {
		t.Error("fallback keyring must not be opened when the native keyring succeeds")
	}
}

// TestBuildDefaultStore_FallsBackToFileKeyring verifies that when the native
// keyring is unavailable (as inside a sandbox), the file-backed keyring is
// used — the regression path for issue #3037.
func TestBuildDefaultStore_FallsBackToFileKeyring(t *testing.T) {
	t.Parallel()
	fallback := keyring.NewArrayKeyring(nil)
	var gotDir string

	store := buildDefaultStore(t.TempDir(),
		func() (keyring.Keyring, error) { return nil, errors.New("no OS keyring") },
		func(dir string) (keyring.Keyring, error) {
			gotDir = dir
			return fallback, nil
		},
	)

	ks, ok := store.(*KeyringTokenStore)
	if !ok {
		t.Fatalf("expected *KeyringTokenStore, got %T", store)
	}
	if ks.ring != fallback {
		t.Error("expected the store to be backed by the fallback keyring")
	}
	if filepath.Base(gotDir) != fallbackKeyringDir {
		t.Errorf("fallback dir = %q, want basename %q", gotDir, fallbackKeyringDir)
	}
}

// TestBuildDefaultStore_FallsBackToMemory verifies the last-resort in-memory
// store is used when neither keyring backend can be opened.
func TestBuildDefaultStore_FallsBackToMemory(t *testing.T) {
	t.Parallel()
	store := buildDefaultStore(t.TempDir(),
		func() (keyring.Keyring, error) { return nil, errors.New("no OS keyring") },
		func(string) (keyring.Keyring, error) { return nil, errors.New("no file keyring") },
	)

	if _, ok := store.(*KeyringTokenStore); ok {
		t.Fatal("expected in-memory store, got *KeyringTokenStore")
	}
}

// TestBuildDefaultStore_NilRingFallsThrough guards against a (nil, nil) return
// from an opener: a nil keyring must be treated like an error and fall through
// to the next tier rather than constructing a store that panics on first use.
func TestBuildDefaultStore_NilRingFallsThrough(t *testing.T) {
	t.Parallel()
	fallback := keyring.NewArrayKeyring(nil)

	// Native opener returns (nil, nil): must not be used, must fall through.
	store := buildDefaultStore(t.TempDir(),
		func() (keyring.Keyring, error) { return nil, nil },
		func(string) (keyring.Keyring, error) { return fallback, nil },
	)
	ks, ok := store.(*KeyringTokenStore)
	if !ok {
		t.Fatalf("expected *KeyringTokenStore, got %T", store)
	}
	if ks.ring != fallback {
		t.Error("a nil native ring must fall through to the fallback keyring")
	}

	// Both openers return (nil, nil): must land on the in-memory store.
	memStore := buildDefaultStore(t.TempDir(),
		func() (keyring.Keyring, error) { return nil, nil },
		func(string) (keyring.Keyring, error) { return nil, nil },
	)
	if _, ok := memStore.(*KeyringTokenStore); ok {
		t.Fatal("a nil fallback ring must fall through to the in-memory store")
	}
}

// TestKeyringTokenStore_ConcurrentAccess guards the store's thread-safety
// contract: its mutex protects the cache/key/loaded fields against
// concurrent GetToken/StoreToken/RemoveToken calls. The op count is kept
// small on purpose — the race detector flags an unsynchronised access on
// the first overlapping pair, so a few overlapping iterations exercise the
// contract just as well as thousands while keeping the test fast. Run under
// -race for it to be meaningful.
func TestKeyringTokenStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)

	const numGoroutines = 3
	const numOperations = 10

	var wg sync.WaitGroup

	for i := range numGoroutines {
		wg.Go(func() {
			url := fmt.Sprintf("https://server-%d.example/mcp", i)
			for range numOperations {
				_, _ = store.GetToken(url)
			}
		})
		wg.Go(func() {
			url := fmt.Sprintf("https://server-%d.example/mcp", i)
			for j := range numOperations {
				_ = store.StoreToken(url, &mcp.OAuthToken{
					AccessToken: fmt.Sprintf("token-%d-%d", i, j),
				})
			}
		})
		wg.Go(func() {
			url := fmt.Sprintf("https://server-%d.example/mcp", i)
			for range numOperations {
				_ = store.RemoveToken(url)
			}
		})
	}

	wg.Wait()

	// The store must still be usable after the concurrent churn.
	_ = store.ListOAuthTokens()
}
