package harness

import (
	"fmt"
	"sync"
)

var (
	regMu    sync.RWMutex
	registry = map[string]HarnessAdapter{}

	tokenMu    sync.Mutex
	tokenInUse = map[string]bool{}
)

// Register registers an adapter by name. Typically called from adapter init() functions.
// Panics if an adapter with the same name is already registered.
func Register(a HarnessAdapter) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := registry[a.Name()]; exists {
		panic(fmt.Sprintf("harness: adapter %q already registered", a.Name()))
	}
	registry[a.Name()] = a
}

// Lookup returns the adapter for the given harness type name.
// Returns an error if no adapter is registered for that name.
func Lookup(name string) (HarnessAdapter, error) {
	regMu.RLock()
	defer regMu.RUnlock()
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("harness: no adapter registered for type %q; valid types: claude-code, codex, opencode, copilot, openclaw", name)
	}
	return a, nil
}

// AcquireToken marks a resume token as in-use for the duration of a sub-session.
// Returns an error if the token is already acquired by another active sub-session.
// Call ReleaseToken when the sub-session ends.
func AcquireToken(token string) error {
	if token == "" {
		return nil
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if tokenInUse[token] {
		return fmt.Errorf("harness: session token %q is already in use by another active sub-session; concurrent reuse is not supported", token)
	}
	tokenInUse[token] = true
	return nil
}

// ReleaseToken marks a resume token as no longer in use.
func ReleaseToken(token string) {
	if token == "" {
		return
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	delete(tokenInUse, token)
}
