package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/browser"
)

// GenerateState generates a random state parameter for OAuth CSRF protection
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BuildAuthorizationURL builds the OAuth authorization URL with PKCE
func BuildAuthorizationURL(authEndpoint, clientID, redirectURI, state, codeChallenge, resourceURL string, scopes []string) string {
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("resource", resourceURL) // RFC 8707: Resource Indicators
	if len(scopes) > 0 {
		params.Set("scope", strings.Join(scopes, " "))
	}
	return authEndpoint + "?" + params.Encode()
}

// ExchangeCodeForToken exchanges an authorization code for an access token
func ExchangeCodeForToken(ctx context.Context, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI string) (*OAuthToken, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)
	data.Set("client_id", clientID)
	data.Set("code_verifier", codeVerifier)
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code for token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var token OAuthToken
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	if token.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}

	return &token, nil
}

// RequestAuthorizationCode requests the user to open the authorization URL and waits for the callback
func RequestAuthorizationCode(ctx context.Context, authURL string, callbackServer *CallbackServer, expectedState string) (string, string, error) {
	if err := browser.Open(ctx, authURL); err != nil {
		return "", "", err
	}

	code, state, err := callbackServer.WaitForCallback(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to receive authorization callback: %w", err)
	}

	if state != expectedState {
		return "", "", fmt.Errorf("state mismatch: expected %s, got %s", expectedState, state)
	}

	return code, state, nil
}

// RegisterClient performs dynamic client registration
func RegisterClient(ctx context.Context, authMetadata *AuthorizationServerMetadata, redirectURI string, scopes []string) (clientID, clientSecret string, err error) {
	if authMetadata.RegistrationEndpoint == "" {
		return "", "", errors.New("authorization server does not support dynamic client registration")
	}

	reqBody := map[string]any{
		"redirect_uris": []string{redirectURI},
		"client_name":   "cagent",
		"grant_types":   []string{"authorization_code"},
		"response_types": []string{
			"code",
		},
	}
	if len(scopes) > 0 {
		reqBody["scope"] = strings.Join(scopes, " ")
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal registration request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authMetadata.RegistrationEndpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return "", "", fmt.Errorf("failed to create registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to register client: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("client registration failed with status %d: %s", resp.StatusCode, string(body))
	}

	var respBody struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return "", "", fmt.Errorf("failed to decode registration response: %w", err)
	}

	if respBody.ClientID == "" {
		return "", "", errors.New("registration response missing client_id")
	}

	return respBody.ClientID, respBody.ClientSecret, nil
}

// GeneratePKCEVerifier generates a PKCE code verifier using oauth2 library
func GeneratePKCEVerifier() string {
	return oauth2.GenerateVerifier()
}
