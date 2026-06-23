package mcp

import "testing"

// resetDefaultStore restores the package-level factory state so each test
// starts from a clean slate and doesn't leak into the others.
func resetDefaultStore(t *testing.T) {
	t.Helper()
	defaultStoreMu.Lock()
	prevFactory, prevStore := defaultFactory, defaultStore
	defaultFactory, defaultStore = NewInMemoryTokenStore, nil
	defaultStoreMu.Unlock()
	t.Cleanup(func() {
		defaultStoreMu.Lock()
		defaultFactory, defaultStore = prevFactory, prevStore
		defaultStoreMu.Unlock()
	})
}

func TestDefaultTokenStore_ReturnsSameInstance(t *testing.T) {
	resetDefaultStore(t)
	if a, b := defaultTokenStore(), defaultTokenStore(); a != b {
		t.Fatal("defaultTokenStore must return the same instance on every call")
	}
}

func TestSetDefaultTokenStoreFactory_UsesFactory(t *testing.T) {
	resetDefaultStore(t)
	want := NewInMemoryTokenStore()
	SetDefaultTokenStoreFactory(func() OAuthTokenStore { return want })
	if got := defaultTokenStore(); got != want {
		t.Fatal("defaultTokenStore must use the registered factory")
	}
}

func TestSetDefaultTokenStoreFactory_NilFallsBackToInMemory(t *testing.T) {
	resetDefaultStore(t)
	SetDefaultTokenStoreFactory(nil)
	if _, ok := defaultTokenStore().(*InMemoryTokenStore); !ok {
		t.Fatal("nil factory must fall back to the in-memory store")
	}
}

func TestSetDefaultTokenStoreFactory_PanicsAfterStoreCreated(t *testing.T) {
	resetDefaultStore(t)
	defaultTokenStore() // force creation

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when setting factory after store creation")
		}
	}()
	SetDefaultTokenStoreFactory(NewInMemoryTokenStore)
}
