package mcp

import (
	"fmt"
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

type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	// UserDeclined is set to true when the user explicitly declines the OAuth
	// authorization prompt. It acts as a session-scoped sentinel that prevents
	// the flow from being re-triggered on subsequent requests.
	UserDeclined bool `json:"user_declined,omitempty"`
}

// IsExpired checks if the token is expired
func (t *OAuthToken) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	// Consider token expired 30 seconds before actual expiry for safety
	return time.Now().Add(30 * time.Second).After(t.ExpiresAt)
}

// InMemoryTokenStore implements OAuthTokenStore in memory
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
