package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	"github.com/docker/docker-agent/pkg/httpclient"
)

// oauthHTTPClient is the *http.Client used for outbound OAuth requests
// (token exchange, refresh, dynamic client registration). The endpoint
// URLs come from MCP server metadata, i.e. effectively the remote
// server's choice — so a hostile MCP server could otherwise redirect us
// at, or hold a connection open to, an internal address. The dialer
// rejects non-public IPs (defeating SSRF and DNS rebinding to loopback /
// link-local / RFC1918 / cloud metadata), and the wall-clock timeout
// puts an upper bound on a slow-loris token endpoint.
//
// Tests in this package replace the var via TestMain (see main_test.go)
// because httptest.NewServer binds to 127.0.0.1.
var oauthHTTPClient = httpclient.NewSafeClient(30*time.Second, false)

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

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code for token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	token, err := parseTokenResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	token.ClientID = clientID
	token.ClientSecret = clientSecret

	return token, nil
}

// tokenResponse is the on-the-wire shape of an OAuth 2.0 token response.
//
// It accepts both:
//
//   - the standard flat shape defined by RFC 6749 §5.1 (access_token, token_type,
//     expires_in, refresh_token at the top level); and
//
//   - Slack's user-token flow (`oauth.v2.user.access`), which returns the user
//     token nested inside an `authed_user` object and signals application-level
//     success/failure with an `ok` boolean and `error` string rather than via
//     HTTP status alone.
//
// Fields that do not exist in one variant are simply left at their zero value.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`

	// Slack application-level status. OK is a pointer so we can distinguish
	// "field absent" (standard OAuth response) from "ok:false" (Slack error).
	OK    *bool  `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`

	// Slack user-token flow nests the actual token under authed_user.
	AuthedUser *struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in,omitempty"`
		RefreshToken string `json:"refresh_token,omitempty"`
		Scope        string `json:"scope,omitempty"`
	} `json:"authed_user,omitempty"`
}

// parseTokenResponse decodes a JSON token response body and normalizes it to
// an OAuthToken, supporting both the standard flat OAuth 2.0 shape and
// Slack's nested `authed_user` shape. It returns an error when the response
// signals `ok:false` or contains no usable access token.
func parseTokenResponse(body io.Reader) (*OAuthToken, error) {
	var resp tokenResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}

	// Slack surfaces application-level failures with HTTP 200 + ok:false.
	if resp.OK != nil && !*resp.OK {
		if resp.Error != "" {
			return nil, fmt.Errorf("token endpoint returned error: %s", resp.Error)
		}
		return nil, errors.New("token endpoint returned ok:false with no error details")
	}

	token := &OAuthToken{
		AccessToken:  resp.AccessToken,
		TokenType:    resp.TokenType,
		ExpiresIn:    resp.ExpiresIn,
		RefreshToken: resp.RefreshToken,
		Scope:        resp.Scope,
	}

	// Fall back to authed_user for providers that nest the user token there
	// (notably Slack's oauth.v2.user.access endpoint).
	if token.AccessToken == "" && resp.AuthedUser != nil && resp.AuthedUser.AccessToken != "" {
		token.AccessToken = resp.AuthedUser.AccessToken
		if token.TokenType == "" {
			token.TokenType = resp.AuthedUser.TokenType
		}
		if token.ExpiresIn == 0 {
			token.ExpiresIn = resp.AuthedUser.ExpiresIn
		}
		if token.RefreshToken == "" {
			token.RefreshToken = resp.AuthedUser.RefreshToken
		}
		if token.Scope == "" {
			token.Scope = resp.AuthedUser.Scope
		}
	}

	if token.AccessToken == "" {
		return nil, errors.New("token response did not contain an access_token")
	}

	if token.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}

	return token, nil
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

	if !constantTimeStateEqual(state, expectedState) {
		return "", "", errors.New("OAuth state mismatch (possible CSRF attempt or stale callback)")
	}

	return code, state, nil
}

// constantTimeStateEqual compares two OAuth state values in constant time to
// avoid leaking the expected value through timing side-channels. It returns
// false when either value is empty so the caller doesn't accept a missing
// expected state as a match.
func constantTimeStateEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// RegisterClient performs dynamic client registration
func RegisterClient(ctx context.Context, authMetadata *AuthorizationServerMetadata, redirectURI string, scopes []string) (clientID, clientSecret string, err error) {
	if authMetadata.RegistrationEndpoint == "" {
		return "", "", errors.New("authorization server does not support dynamic client registration")
	}

	reqBody := map[string]any{
		"redirect_uris": []string{redirectURI},
		"client_name":   "docker-agent",
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

	resp, err := oauthHTTPClient.Do(req)
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

// RefreshAccessToken uses a refresh token to obtain a new access token
// without user interaction.
func RefreshAccessToken(ctx context.Context, tokenEndpoint, refreshToken, clientID, clientSecret string) (*OAuthToken, error) {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("client_id", clientID)
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	token, err := parseTokenResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode refresh response: %w", err)
	}

	// Preserve the refresh token if the server didn't issue a new one
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}

	// Preserve client credentials so subsequent refreshes work
	token.ClientID = clientID
	token.ClientSecret = clientSecret

	return token, nil
}
