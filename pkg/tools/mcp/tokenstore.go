package mcp

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
)

// OAuthTokenStore manages OAuth tokens
type OAuthTokenStore interface {
	// GetToken retrieves a token for the given resource URL
	GetToken(resourceURL string) (*OAuthToken, error)
	// StoreToken stores a token for the given resource URL
	StoreToken(resourceURL string, token *OAuthToken) error
	// RemoveToken removes a token for the given resource URL
	RemoveToken(resourceURL string) error
}

// OAuthTokenStoreFactory constructs the process-wide persistent OAuth token store.
type OAuthTokenStoreFactory func() OAuthTokenStore

var (
	defaultStoreMu sync.Mutex
	defaultFactory OAuthTokenStoreFactory = NewInMemoryTokenStore
	defaultStore   OAuthTokenStore
)

// SetDefaultTokenStoreFactory installs the process-wide persistent token-store
// factory used by NewKeyringTokenStore and remote MCP toolsets. Embedders that
// do not need persistent MCP OAuth storage can avoid importing any OS keyring
// implementation; docker-agent's CLI registers one from pkg/tools/mcp/keyringstore.
//
// It must be called before the default store is first constructed (i.e. before
// any NewKeyringTokenStore call); doing otherwise would leave early callers
// holding a different store instance than later ones, so it panics to surface
// the misordering instead of silently diverging.
func SetDefaultTokenStoreFactory(factory OAuthTokenStoreFactory) {
	if factory == nil {
		factory = NewInMemoryTokenStore
	}
	defaultStoreMu.Lock()
	defer defaultStoreMu.Unlock()
	if defaultStore != nil {
		panic("mcp: SetDefaultTokenStoreFactory called after the default token store was already created")
	}
	defaultFactory = factory
}

// defaultTokenStore lazily builds the process-wide store and returns the same
// instance to every caller. The factory runs under the mutex so a concurrent
// SetDefaultTokenStoreFactory can never hand out a second, divergent store.
func defaultTokenStore() OAuthTokenStore {
	defaultStoreMu.Lock()
	defer defaultStoreMu.Unlock()
	if defaultStore == nil {
		defaultStore = defaultFactory()
	}
	return defaultStore
}

// NewKeyringTokenStore returns the process-wide persistent OAuth token store.
// The name is kept for compatibility; without an optional keyring-backed store
// registered by pkg/tools/mcp/keyringstore, it falls back to an in-memory store.
func NewKeyringTokenStore() OAuthTokenStore {
	return defaultTokenStore()
}

type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AuthServer   string    `json:"auth_server,omitempty"`

	// RequestedScopes records the scope list the config asked for when this
	// token was obtained. Unlike Scope (which is whatever the authorization
	// server chose to return, sometimes empty, sometimes comma/space
	// separated), RequestedScopes reflects our intent and is used to detect
	// when the config has changed and a new OAuth flow is required.
	RequestedScopes []string `json:"requested_scopes,omitempty"`
}

// IsExpired checks if the token is expired
func (t *OAuthToken) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	// Consider token expired 30 seconds before actual expiry for safety
	return time.Now().Add(30 * time.Second).After(t.ExpiresAt)
}

// OAuthTokenEntry pairs a stored OAuth token with its resource URL.
type OAuthTokenEntry struct {
	ResourceURL string
	Token       *OAuthToken
}

type oauthTokenLister interface {
	ListOAuthTokens() []OAuthTokenEntry
}

// ListOAuthTokens returns every OAuth token from the registered persistent store.
func ListOAuthTokens() ([]OAuthTokenEntry, error) {
	store := NewKeyringTokenStore()
	lister, ok := store.(oauthTokenLister)
	if !ok {
		return nil, errors.New("persistent OAuth token store not available")
	}
	return lister.ListOAuthTokens(), nil
}

// RemoveOAuthToken deletes the token stored for resourceURL.
func RemoveOAuthToken(resourceURL string) error {
	return NewKeyringTokenStore().RemoveToken(resourceURL)
}

type InMemoryTokenStore struct {
	tokens *concurrent.Map[string, *OAuthToken]
}

// NewInMemoryTokenStore creates a new in-memory token store
func NewInMemoryTokenStore() OAuthTokenStore {
	return &InMemoryTokenStore{
		tokens: concurrent.NewMap[string, *OAuthToken](),
	}
}

func (s *InMemoryTokenStore) GetToken(resourceURL string) (*OAuthToken, error) {
	token, ok := s.tokens.Load(resourceURL)
	if !ok {
		return nil, fmt.Errorf("no token found for resource: %s", resourceURL)
	}
	return token, nil
}

func (s *InMemoryTokenStore) StoreToken(resourceURL string, token *OAuthToken) error {
	s.tokens.Store(resourceURL, token)
	return nil
}

func (s *InMemoryTokenStore) RemoveToken(resourceURL string) error {
	s.tokens.Delete(resourceURL)
	return nil
}
