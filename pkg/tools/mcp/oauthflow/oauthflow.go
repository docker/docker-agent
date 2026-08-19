// Package oauthflow implements the client side of the OAuth 2.0
// authorization-code flow used to authenticate against remote MCP servers:
// PKCE and state generation, the loopback callback server, dynamic client
// registration, and code/refresh token exchange.
//
// It is deliberately separate from pkg/tools/mcp so that packages which only
// drive the flow (e.g. pkg/runtime's remote-runtime login) don't link the
// whole MCP toolset - and everything it drags in - into embedders' binaries.
package oauthflow

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
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/httpclient"
)

// maxErrorBodyBytes caps how much of a non-200 response body is read for
// error messages. The body comes from a server the user pointed us at, but
// errors end up in logs; a bound keeps a hostile or misconfigured endpoint
// from stuffing megabytes (or secrets past the first error fields) into them.
const maxErrorBodyBytes = 512

// defaultHTTPClient is the *http.Client used for outbound OAuth requests
// (metadata discovery, token exchange, refresh, dynamic client registration).
// The endpoint URLs come from MCP server metadata, i.e. effectively the remote
// server's choice — so a hostile MCP server could otherwise redirect us
// at, or hold a connection open to, an internal address. The dialer
// rejects non-public IPs (defeating SSRF and DNS rebinding to loopback /
// link-local / RFC1918 / cloud metadata), and the wall-clock timeout
// puts an upper bound on a slow-loris token endpoint.
//
// It is deliberately not exported as a plain var: only tests may replace it
// (see SetHTTPClientForTesting), so embedders can't accidentally disable the
// SSRF protection process-wide.
var defaultHTTPClient = func() *atomic.Pointer[http.Client] {
	p := &atomic.Pointer[http.Client]{}
	p.Store(httpclient.NewSafeClient(30*time.Second, false))
	return p
}()

// DefaultHTTPClient returns the shared SSRF-safe client used for OAuth
// requests that don't carry per-server header or private-IP policies.
func DefaultHTTPClient() *http.Client {
	return defaultHTTPClient.Load()
}

// SetHTTPClientForTesting replaces the shared OAuth client and returns a
// function restoring the previous one. It exists only so tests can hit
// httptest servers, which bind to 127.0.0.1 — an address the SSRF-safe
// client refuses by design. Never call it in production code.
func SetHTTPClientForTesting(c *http.Client) (restore func()) {
	prev := defaultHTTPClient.Swap(c)
	return func() { defaultHTTPClient.Store(prev) }
}

// BrowserOpener opens urlToOpen in the user's default browser. It is the
// seam through which RequestAuthorizationCode delivers the authorization URL;
// tests replace it to capture the URL without launching a real browser.
type BrowserOpener func(ctx context.Context, urlToOpen string) error

// browserOpener is the process-wide opener. Production uses browser.Open;
// tests replace it via SetBrowserOpenerForTesting.
var browserOpener = func() *atomic.Pointer[BrowserOpener] {
	p := &atomic.Pointer[BrowserOpener]{}
	def := BrowserOpener(browser.Open)
	p.Store(&def)
	return p
}()

// SetBrowserOpenerForTesting replaces the browser opener and returns a
// function restoring the previous one. It exists only so tests can intercept
// the authorization URL without launching a real browser (and without relying
// on host-OS launchers). Never call it in production code.
func SetBrowserOpenerForTesting(opener BrowserOpener) (restore func()) {
	prev := browserOpener.Swap(&opener)
	return func() { browserOpener.Store(prev) }
}

// HTTPClientForAllowPrivateIPs returns the shared SSRF-safe client, or a
// variant that may reach private IPs when allowPrivateIPs is set (used by
// configurations that explicitly opt in to talking to a server on a private
// network).
func HTTPClientForAllowPrivateIPs(allowPrivateIPs bool) *http.Client {
	if allowPrivateIPs {
		// Clone keeps the default proxy/HTTP2/timeout behavior but gives the
		// client its own connection pool: a nil Transport would share
		// http.DefaultTransport's pool, which third parties may prune via
		// CloseIdleConnections (httptest.Server.Close does).
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			// Something replaced DefaultTransport (test helper, proxy shim);
			// fall back to a fresh default rather than panicking.
			transport = &http.Transport{}
		}
		return &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport.Clone(),
		}
	}
	return DefaultHTTPClient()
}

// GenerateState generates a random state parameter for OAuth CSRF protection
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BuildAuthorizationURL builds the OAuth authorization URL with PKCE.
// It merges the OAuth parameters into any query string already present on
// authEndpoint (e.g. Grafana Cloud's grafana_url= selector) so the result
// always has exactly one '?' even when the endpoint carries pre-existing params.
func BuildAuthorizationURL(authEndpoint, clientID, redirectURI, state, codeChallenge, resourceURL string, scopes []string) string {
	u, err := url.Parse(authEndpoint)
	if err != nil {
		// Degrade gracefully: treat the whole input as an opaque endpoint.
		u = &url.URL{Path: authEndpoint}
	}
	q := u.Query() // preserves any params the auth server baked into the endpoint (e.g. grafana_url)
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resourceURL) // RFC 8707: Resource Indicators
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ExchangeCodeForToken exchanges an authorization code for an access token.
func ExchangeCodeForToken(ctx context.Context, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI string) (*OAuthToken, error) {
	return ExchangeCodeForTokenWithClient(ctx, DefaultHTTPClient(), tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI, "")
}

// ExchangeCodeForTokenWithResource exchanges an authorization code and sends
// the RFC 8707 resource indicator to token endpoints that require it.
func ExchangeCodeForTokenWithResource(ctx context.Context, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI, resourceURL string) (*OAuthToken, error) {
	return ExchangeCodeForTokenWithClient(ctx, DefaultHTTPClient(), tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI, resourceURL)
}

// ExchangeCodeForTokenWithClient is ExchangeCodeForTokenWithResource with an
// explicit *http.Client, for callers that layer per-server headers or
// private-IP policies on top of the default client.
func ExchangeCodeForTokenWithClient(ctx context.Context, client *http.Client, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI, resourceURL string) (*OAuthToken, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)
	data.Set("client_id", clientID)
	data.Set("code_verifier", codeVerifier)
	if resourceURL != "" {
		data.Set("resource", resourceURL)
	}
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code for token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
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
	if err := (*browserOpener.Load())(ctx, authURL); err != nil {
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
	return RegisterClientWithClient(ctx, DefaultHTTPClient(), authMetadata, redirectURI, scopes)
}

// RegisterClientWithClient is RegisterClient with an explicit *http.Client.
func RegisterClientWithClient(ctx context.Context, client *http.Client, authMetadata *AuthorizationServerMetadata, redirectURI string, scopes []string) (clientID, clientSecret string, err error) {
	if authMetadata.RegistrationEndpoint == "" {
		return "", "", errors.New("authorization server does not support dynamic client registration")
	}

	reqBody := map[string]any{
		"redirect_uris":  []string{redirectURI},
		"client_name":    "docker-agent",
		"grant_types":    []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"},
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

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to register client: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
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
	return RefreshAccessTokenWithClient(ctx, DefaultHTTPClient(), tokenEndpoint, refreshToken, clientID, clientSecret)
}

// RefreshAccessTokenWithClient is RefreshAccessToken with an explicit *http.Client.
func RefreshAccessTokenWithClient(ctx context.Context, client *http.Client, tokenEndpoint, refreshToken, clientID, clientSecret string) (*OAuthToken, error) {
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

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
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
