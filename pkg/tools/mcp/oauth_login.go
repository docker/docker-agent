package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

// PerformOAuthLogin performs a standalone OAuth flow for the given MCP server URL.
// It discovers the authorization server metadata, performs dynamic client registration,
// opens the browser for user authorization, and stores the resulting token in the keyring.
func PerformOAuthLogin(ctx context.Context, serverURL string) error {
	tokenStore := NewKeyringTokenStore()

	o := &oauth{metadataClient: &http.Client{Timeout: 5 * time.Second}}

	// Derive the base origin (scheme + host) from the server URL.
	// The well-known endpoints live at the origin, not under the SSE/path.
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	baseURL := parsed.Scheme + "://" + parsed.Host

	// Discover protected resource metadata.
	resourceURL := baseURL + "/.well-known/oauth-protected-resource"
	resourceReq, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("failed to create resource metadata request: %w", err)
	}
	resp, err := http.DefaultClient.Do(resourceReq)
	if err != nil {
		return fmt.Errorf("failed to fetch protected resource metadata: %w", err)
	}
	defer resp.Body.Close()

	authServer := baseURL
	if resp.StatusCode == http.StatusOK {
		var resourceMetadata protectedResourceMetadata
		if decErr := json.NewDecoder(resp.Body).Decode(&resourceMetadata); decErr == nil && len(resourceMetadata.AuthorizationServers) > 0 {
			authServer = resourceMetadata.AuthorizationServers[0]
		}
	}

	// Discover authorization server metadata.
	authServerMetadata, err := o.getAuthorizationServerMetadata(ctx, authServer)
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	// Set up the callback server for the redirect.
	callbackServer, err := NewCallbackServer()
	if err != nil {
		return fmt.Errorf("failed to create callback server: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Failed to shutdown callback server", "error", err)
		}
	}()

	if err := callbackServer.Start(); err != nil {
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.GetRedirectURI()

	// Dynamic client registration.
	var clientID, clientSecret string
	if authServerMetadata.RegistrationEndpoint != "" {
		clientID, clientSecret, err = RegisterClient(ctx, authServerMetadata, redirectURI, nil)
		if err != nil {
			return fmt.Errorf("dynamic client registration failed: %w", err)
		}
	} else {
		return errors.New("authorization server does not support dynamic client registration")
	}

	// Generate PKCE and state.
	state, err := GenerateState()
	if err != nil {
		return fmt.Errorf("failed to generate state: %w", err)
	}
	callbackServer.SetExpectedState(state)
	verifier := GeneratePKCEVerifier()

	authURL := BuildAuthorizationURL(
		authServerMetadata.AuthorizationEndpoint,
		clientID,
		redirectURI,
		state,
		oauth2.S256ChallengeFromVerifier(verifier),
		serverURL,
		nil,
	)

	// Open the browser and wait for the callback.
	code, receivedState, err := RequestAuthorizationCode(ctx, authURL, callbackServer, state)
	if err != nil {
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		return errors.New("state mismatch in authorization response")
	}

	// Exchange the code for a token.
	token, err := ExchangeCodeForToken(ctx, authServerMetadata.TokenEndpoint, code, verifier, clientID, clientSecret, redirectURI)
	if err != nil {
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	token.ClientID = clientID
	token.ClientSecret = clientSecret

	if err := tokenStore.StoreToken(serverURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	return nil
}
