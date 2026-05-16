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
