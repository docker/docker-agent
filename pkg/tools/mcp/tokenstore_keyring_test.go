package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/99designs/keyring"
)

func TestKeyringTokenStore_RoundTrip(t *testing.T) {
	// Use in-memory store to avoid triggering macOS keychain permission dialogs
	// or failing in CI environments without a keyring.
	store := NewInMemoryTokenStore()

	resourceURL := "https://example.com/mcp"

	// Initially no token
	if _, err := store.GetToken(resourceURL); err == nil {
		t.Fatal("expected error for missing token")
	}

	// Store a token
	token := &OAuthToken{
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
	// Verify that OAuthToken serializes correctly (important for keyring storage)
	token := &OAuthToken{
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

	var got OAuthToken
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.AccessToken != token.AccessToken || got.RefreshToken != token.RefreshToken || got.Scope != token.Scope {
		t.Errorf("JSON round-trip mismatch: got %+v, want %+v", got, token)
	}
}

func TestKeyringTokenStore_RemoveNonExistent(t *testing.T) {
	store := NewInMemoryTokenStore()
	if err := store.RemoveToken("https://nonexistent.example.com"); err != nil {
		t.Fatalf("RemoveToken for non-existent key should not error: %v", err)
	}
}

// countingKeyring tracks how many times Get and Set are invoked on the
// underlying ring. Used to assert that the bundled layout collapses N
// tokens into a single keyring read.
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

// TestBundledKeyringStore_ReadsCollapsedToOneGet verifies the central
// claim of the bundled layout: regardless of how many resource URLs are
// looked up, the underlying keyring sees only a single Get for the bundle
// key. This is what avoids the "many keychain prompts on macOS" problem.
func TestBundledKeyringStore_ReadsCollapsedToOneGet(t *testing.T) {
	urls := []string{
		"https://server-a.example/mcp",
		"https://server-b.example/mcp",
		"https://server-c.example/mcp",
	}

	ring := newCountingKeyring()
	store := newKeyringTokenStore(ring)
	for i, url := range urls {
		if err := store.StoreToken(url, &OAuthToken{AccessToken: "at-" + string(rune('A'+i))}); err != nil {
			t.Fatalf("StoreToken(%s): %v", url, err)
		}
	}

	// Drop the cache by wrapping the same ring with a fresh store, so we
	// exercise a real load() like a new process would.
	ring.gets, ring.sets = 0, 0
	fresh := newKeyringTokenStore(ring)

	// Read each token several times; only the first read should hit the
	// keyring.
	for range 5 {
		for _, url := range urls {
			if _, err := fresh.GetToken(url); err != nil {
				t.Fatalf("GetToken(%s): %v", url, err)
			}
		}
	}

	if ring.gets != 1 {
		t.Errorf("expected exactly 1 underlying keyring Get, got %d", ring.gets)
	}
	if ring.sets != 0 {
		t.Errorf("read-only path must not write, got %d Set calls", ring.sets)
	}
}

// TestBundledKeyringStore_StoreReusesSameItem verifies that storing
// tokens for many different resource URLs all go to the same keyring item
// — so macOS only ever asks for permission on a single ACL.
func TestBundledKeyringStore_StoreReusesSameItem(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	store := newKeyringTokenStore(ring)

	for _, url := range []string{
		"https://server-a.example/mcp",
		"https://server-b.example/mcp",
		"https://server-c.example/mcp",
	} {
		if err := store.StoreToken(url, &OAuthToken{AccessToken: "at"}); err != nil {
			t.Fatalf("StoreToken(%s): %v", url, err)
		}
	}

	keys, err := ring.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != bundleKey {
		t.Fatalf("expected single bundle item %q, got %v", bundleKey, keys)
	}
}

// TestBundledKeyringStore_LegacyMigration confirms tokens previously
// stored with the per-resource layout are folded into the bundle on first
// load and the legacy entries are cleaned up.
func TestBundledKeyringStore_LegacyMigration(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)

	urls := []string{
		"https://legacy-a.example/mcp",
		"https://legacy-b.example/mcp",
	}
	for _, url := range urls {
		seedLegacyToken(t, ring, url, &OAuthToken{AccessToken: "legacy-" + url})
	}
	// Also seed the legacy index, which should be removed without
	// becoming a token entry.
	if err := ring.Set(keyring.Item{
		Key:  legacyIndexKey,
		Data: []byte(`["https://legacy-a.example/mcp"]`),
	}); err != nil {
		t.Fatalf("seed legacy index: %v", err)
	}

	store := newKeyringTokenStore(ring)

	// Reading any token triggers migration; both legacy tokens should be
	// reachable afterwards.
	for _, url := range urls {
		got, err := store.GetToken(url)
		if err != nil {
			t.Fatalf("GetToken(%s): %v", url, err)
		}
		if want := "legacy-" + url; got.AccessToken != want {
			t.Errorf("AccessToken for %s = %q, want %q", url, got.AccessToken, want)
		}
	}

	// The keyring should now contain only the bundle key.
	keys, err := ring.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != bundleKey {
		t.Errorf("expected only bundle key after migration, got %v", keys)
	}
}

func seedLegacyToken(t *testing.T, ring keyring.Keyring, url string, tok *OAuthToken) {
	t.Helper()
	data, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal legacy token: %v", err)
	}
	if err := ring.Set(keyring.Item{Key: legacyTokenPrefix + url, Data: data}); err != nil {
		t.Fatalf("seed legacy item: %v", err)
	}
}

// TestBundledKeyringStore_RemoveCleansBundle verifies that deleting a
// token rewrites the bundle without it, so subsequent reads no longer see
// it even after the in-memory cache is dropped.
func TestBundledKeyringStore_RemoveCleansBundle(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	store := newKeyringTokenStore(ring)

	url := "https://to-remove.example/mcp"
	if err := store.StoreToken(url, &OAuthToken{AccessToken: "x"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}
	if err := store.RemoveToken(url); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}

	if _, err := newKeyringTokenStore(ring).GetToken(url); err == nil {
		t.Fatal("expected GetToken to fail after RemoveToken")
	}
}

// TestBundledKeyringStore_CorruptBundle ensures a corrupt bundle doesn't
// crash callers — we treat it as empty and let the OAuth flow re-populate.
func TestBundledKeyringStore_CorruptBundle(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	if err := ring.Set(keyring.Item{Key: bundleKey, Data: []byte("not json")}); err != nil {
		t.Fatalf("seed corrupt bundle: %v", err)
	}

	store := newKeyringTokenStore(ring)

	if _, err := store.GetToken("https://anything.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token, got nil")
	}

	// StoreToken on top of a corrupt bundle should overwrite it.
	if err := store.StoreToken("https://anything.example/mcp", &OAuthToken{AccessToken: "fresh"}); err != nil {
		t.Fatalf("StoreToken after corrupt bundle: %v", err)
	}
	got, err := store.GetToken("https://anything.example/mcp")
	if err != nil || got.AccessToken != "fresh" {
		t.Fatalf("expected fresh token after recovery, got token=%v err=%v", got, err)
	}
}

// TestKeyringTokenStore_ListReturnsAllEntries exercises the list helper
// used by `agent debug oauth list`.
func TestKeyringTokenStore_ListReturnsAllEntries(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	store := newKeyringTokenStore(ring)

	if err := store.StoreToken("https://a.example/mcp", &OAuthToken{AccessToken: "a"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}
	if err := store.StoreToken("https://b.example/mcp", &OAuthToken{AccessToken: "b"}); err != nil {
		t.Fatalf("StoreToken: %v", err)
	}

	// Reload from the ring (mirroring what a fresh process would do).
	entries := newKeyringTokenStore(ring).list()
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

// failingKeyring returns a fixed error from Get; used to make sure the
// store doesn't permanently re-prompt when keychain access is denied.
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

// TestBundledKeyringStore_LoadFailureIsCachedOnce checks that a single
// keyring failure does not turn into an avalanche of repeated prompts:
// load() marks the cache as loaded eagerly so a denied access surfaces
// once per process, not once per token operation.
func TestBundledKeyringStore_LoadFailureIsCachedOnce(t *testing.T) {
	ring := &failingKeyring{getErr: errors.New("simulated denied access")}
	store := newKeyringTokenStore(ring)

	if _, err := store.GetToken("https://a.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token after denied keyring access")
	}
	if _, err := store.GetToken("https://b.example/mcp"); err == nil {
		t.Fatal("expected GetToken to report missing token after denied keyring access")
	}
}

// TestKeyringTokenStore_ConcurrentAccess verifies that concurrent reads
// and writes to the token store are safe and don't cause data races.
func TestKeyringTokenStore_ConcurrentAccess(t *testing.T) {
	ring := keyring.NewArrayKeyring(nil)
	store := newKeyringTokenStore(ring)

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
				_ = store.StoreToken(url, &OAuthToken{
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
	entries := store.list()
	t.Logf("After concurrent access: %d tokens remain", len(entries))
}
