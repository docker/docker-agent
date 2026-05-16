package mcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// resourceMetadataFromWWWAuth extracts resource metadata URL from WWW-Authenticate header
var re = regexp.MustCompile(`resource="([^"]+)"`)

// oauth is a simple struct for compatibility with existing code
type oauth struct {
	metadataClient *http.Client
}

// protectedResourceMetadata represents OAuth 2.0 Protected Resource Metadata (RFC 8707)
type protectedResourceMetadata struct {
	Resource                          string   `json:"resource"`
	AuthorizationServers              []string `json:"authorization_servers"`
	ResourceName                      string   `json:"resource_name,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported            []string `json:"bearer_methods_supported,omitempty"`
	ResourceSigningAlgValuesSupported []string `json:"resource_signing_alg_values_supported,omitempty"`
}

// AuthorizationServerMetadata represents OAuth 2.0 Authorization Server Metadata (RFC 8414)
type AuthorizationServerMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint                     string   `json:"revocation_endpoint,omitempty"`
	IntrospectionEndpoint                  string   `json:"introspection_endpoint,omitempty"`
	JwksURI                                string   `json:"jwks_uri,omitempty"`
	ScopesSupported                        []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	ResponseModesSupported                 []string `json:"response_modes_supported,omitempty"`
	GrantTypesSupported                    []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported,omitempty"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported,omitempty"`
}

func (o *oauth) getAuthorizationServerMetadata(ctx context.Context, authServerURL string) (*AuthorizationServerMetadata, error) {
	// Build well-known metadata URL
	metadataURL := authServerURL
	if !strings.HasSuffix(authServerURL, "/.well-known/oauth-authorization-server") {
		metadataURL = strings.TrimSuffix(authServerURL, "/") + "/.well-known/oauth-authorization-server"
	}

	// Attempt OAuth authorization server discovery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := o.metadataClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Try OpenID Connect discovery as fallback
		openIDURL := strings.Replace(metadataURL, "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration", 1)
		oidcReq, err := http.NewRequestWithContext(ctx, http.MethodGet, openIDURL, http.NoBody)
		if err != nil {
			return nil, err
		}
		oidcReq.Header.Set("Accept", "application/json")

		oidcResp, err := o.metadataClient.Do(oidcReq)
		if err != nil {
			return nil, err
		}
		defer oidcResp.Body.Close()

		if oidcResp.StatusCode != http.StatusOK {
			// Return default metadata if all discovery fails
			return createDefaultMetadata(authServerURL), nil
		}

		var metadata AuthorizationServerMetadata
		if err := json.NewDecoder(oidcResp.Body).Decode(&metadata); err != nil {
			return nil, fmt.Errorf("failed to decode metadata from %s: %w", openIDURL, err)
		}
		return validateAndFillDefaults(&metadata, authServerURL), nil
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, metadataURL)
	}

	var metadata AuthorizationServerMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata from %s: %w", metadataURL, err)
	}

	return validateAndFillDefaults(&metadata, authServerURL), nil
}

// validateAndFillDefaults validates required fields and fills in defaults
func validateAndFillDefaults(metadata *AuthorizationServerMetadata, authServerURL string) *AuthorizationServerMetadata {
	metadata.Issuer = cmp.Or(metadata.Issuer, authServerURL)
	if len(metadata.ResponseTypesSupported) == 0 {
		metadata.ResponseTypesSupported = []string{"code"}
	}

	if len(metadata.ResponseModesSupported) == 0 {
		metadata.ResponseModesSupported = []string{"query", "fragment"}
	}
	if len(metadata.GrantTypesSupported) == 0 {
		metadata.GrantTypesSupported = []string{"authorization_code", "implicit"}
	}
	if len(metadata.TokenEndpointAuthMethodsSupported) == 0 {
		metadata.TokenEndpointAuthMethodsSupported = []string{"client_secret_basic"}
	}
	if len(metadata.RevocationEndpointAuthMethodsSupported) == 0 {
		metadata.RevocationEndpointAuthMethodsSupported = []string{"client_secret_basic"}
	}

	metadata.AuthorizationEndpoint = cmp.Or(metadata.AuthorizationEndpoint, authServerURL+"/authorize")
	metadata.TokenEndpoint = cmp.Or(metadata.TokenEndpoint, authServerURL+"/token")
	// Do NOT fabricate a registration_endpoint — if the server doesn't
	// advertise one, dynamic client registration is not supported.

	return metadata
}

// createDefaultMetadata creates minimal metadata when discovery fails
func createDefaultMetadata(authServerURL string) *AuthorizationServerMetadata {
	return &AuthorizationServerMetadata{
		Issuer:                                 authServerURL,
		AuthorizationEndpoint:                  authServerURL + "/authorize",
		TokenEndpoint:                          authServerURL + "/token",
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query", "fragment"},
		GrantTypesSupported:                    []string{"authorization_code"},
		TokenEndpointAuthMethodsSupported:      []string{"client_secret_basic"},
		RevocationEndpointAuthMethodsSupported: []string{"client_secret_basic"},
		CodeChallengeMethodsSupported:          []string{"S256"},
	}
}

func resourceMetadataFromWWWAuth(wwwAuth string) string {
	matches := re.FindStringSubmatch(wwwAuth)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}

// callbackRedirectURLFrom is a nil-safe accessor for the optional
// CallbackRedirectURL field on a RemoteOAuthConfig.
func callbackRedirectURLFrom(c *latest.RemoteOAuthConfig) string {
	if c == nil {
		return ""
	}
	return c.CallbackRedirectURL
}

// oauthTransport wraps an HTTP transport with OAuth support
type oauthTransport struct {
	base http.RoundTripper
	// TODO(rumpl): remove client reference, we need to find a better way to send elicitation requests
	client      *remoteMCPClient
	tokenStore  OAuthTokenStore
	baseURL     string
	managed     bool
	oauthConfig *latest.RemoteOAuthConfig

	// mu protects refreshFailedAt and lastErr* from concurrent access.
	mu sync.Mutex
	// refreshFailedAt tracks the last time a silent token refresh failed,
	// so we avoid retrying on every request.
	refreshFailedAt time.Time
	// lastErrStatus and lastErrBody capture the status code and (truncated)
	// response body of the most recent non-2xx HTTP response received by the
	// transport. They're read by callers of Initialize() to enrich bubbled-up
	// errors with the server's own explanation, which the MCP SDK otherwise
	// swallows in favor of a bare http.StatusText.
	lastErrStatus int
	lastErrBody   []byte
	// lastAuthRequired records when the transport short-circuited an
	// interactive OAuth flow because the request context disallowed
	// prompts (see WithoutInteractivePrompts). The MCP SDK wraps transport
	// errors with %v, breaking errors.As, so callers must use this field
	// instead of unwrapping to know that OAuth was deferred rather than
	// failed for some other reason.
	lastAuthRequired bool
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.roundTrip(req, false)
}

func (t *oauthTransport) roundTrip(req *http.Request, isRetry bool) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
	}

	reqClone := req.Clone(req.Context())

	// Attach a valid token if available, silently refreshing if expired.
	if token := t.getValidToken(req.Context()); token != nil {
		reqClone.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}

	resp, err := t.base.RoundTrip(reqClone)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && !isRetry {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if wwwAuth != "" {
			// If the caller asked for non-interactive operation (e.g. the
			// runtime is populating sidebar tool counts during startup),
			// don't block on an OAuth elicitation that the TUI is not yet
			// ready to surface. Surface a recognisable error instead so
			// the toolset can be flagged "needs auth" without freezing
			// the agent and without making Ctrl-C wait for a user response
			// that will never come.
			if !interactivePromptsAllowed(req.Context()) {
				slog.Debug("Skipping OAuth elicitation in non-interactive context", "url", t.baseURL)
				resp.Body.Close()
				t.mu.Lock()
				t.lastAuthRequired = true
				t.mu.Unlock()
				return nil, &AuthorizationRequiredError{URL: t.baseURL}
			}

			resp.Body.Close()

			authServer := req.URL.Scheme + "://" + req.URL.Host
			if err := t.handleOAuthFlow(req.Context(), authServer, wwwAuth); err != nil {
				// requestElicitation surfaces a bare AuthorizationRequiredError
				// when the runtime hasn't wired up the elicitation bridge yet,
				// which normally means OAuth was triggered too early (the
				// WithoutInteractivePrompts marker would have caught this case
				// at the top of roundTrip, but a missing marker shouldn't
				// translate into a scary "no elicitation handler configured"
				// for the user). Treat the same as the explicit non-interactive
				// path: flag the toolset as needing auth and let it retry on
				// the next conversation turn with a properly-wired bridge.
				var authErr *AuthorizationRequiredError
				if errors.As(err, &authErr) {
					slog.Debug("OAuth flow deferred: elicitation bridge not ready", "url", t.baseURL)
					if authErr.URL == "" {
						authErr.URL = t.baseURL
					}
					t.mu.Lock()
					t.lastAuthRequired = true
					t.mu.Unlock()
					return nil, authErr
				}
				return nil, fmt.Errorf("OAuth flow failed: %w", err)
			}

			if len(bodyBytes) > 0 {
				req.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			}

			return t.roundTrip(req, true)
		}
	}

	// On the authenticated retry, log the response body when the server
	// rejects us with an error status. Otherwise the failure bubbles up as
	// a generic "Bad Request" / "Forbidden" / ... with no detail, making it
	// very hard to understand why the server refused the token we just
	// obtained (e.g. a scope mismatch, insufficient permissions, or
	// provider-specific payload complaints).
	//
	// We also log on the first attempt when the status is something other
	// than a plain 401 we're going to handle via OAuth. In particular, some
	// servers return a non-standard 400 instead of 401 when a stored token
	// is no longer accepted (for example, Slack's MCP endpoint answers
	// `400 Bad Request` with a JSON-RPC error payload when the app has
	// lost access — "App is not enabled for Slack MCP server access"),
	// and surfacing the body is the only way to see the real cause.
	if resp.StatusCode >= 400 {
		t.logErrorResponse(req, resp)
	}

	return resp, nil
}

// logErrorResponse peeks at an error response body (up to a reasonable cap)
// and logs it so the user can see what the server is actually complaining
// about, without preventing the caller from reading the body themselves.
//
// Many MCP server failures come back as short JSON-RPC error envelopes
// (e.g. `{"error":{"code":-32000,"message":"insufficient_scope"}}`) that
// are invaluable for diagnosis but are otherwise swallowed by the MCP SDK
// which only surfaces `http.StatusText(resp.StatusCode)`.
func (t *oauthTransport) logErrorResponse(req *http.Request, resp *http.Response) {
	const maxBody = 2048

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		slog.Warn("Authenticated MCP request failed; could not read response body",
			"url", req.URL.String(),
			"status", resp.StatusCode,
			"error", err,
		)
		// Ensure the body reader is in a usable state for the caller.
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}

	// Drain and replace the body so downstream consumers can still read it.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))

	// Remember the last server-side failure so higher layers (Initialize /
	// doStart) can enrich their error with a human-readable explanation
	// rather than the SDK's bare "Bad Request".
	t.mu.Lock()
	t.lastErrStatus = resp.StatusCode
	t.lastErrBody = body
	t.mu.Unlock()

	slog.Warn("Authenticated MCP request was rejected by the server",
		"url", req.URL.String(),
		"status", resp.StatusCode,
		"www_authenticate", resp.Header.Get("WWW-Authenticate"),
		"content_type", resp.Header.Get("Content-Type"),
		"body", string(body),
	)
}

// lastServerError returns the status code and a short, human-readable
// explanation drawn from the most recent non-2xx response seen by this
// transport. The string is empty when no such response has been captured
// or when the body yielded no useful text.
//
// This is how the transport surfaces provider-specific errors (e.g. Slack's
// "App is not enabled for Slack MCP server access") that would otherwise
// be hidden behind the MCP SDK's generic http.StatusText-derived messages.
func (t *oauthTransport) lastServerError() (int, string) {
	t.mu.Lock()
	status := t.lastErrStatus
	body := t.lastErrBody
	t.mu.Unlock()
	if status == 0 {
		return 0, ""
	}
	return status, extractServerMessage(body)
}

// authorizationRequired reports whether the transport short-circuited an
// interactive OAuth flow because the request context disallowed prompts.
// Callers can use this to recognise the deferred-OAuth case even though
// the MCP SDK destroys the underlying error chain by wrapping with %v.
func (t *oauthTransport) authorizationRequired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastAuthRequired
}

// extractServerMessage extracts a short, human-readable message from a
// server response body. It tries, in order:
//
//  1. A JSON-RPC envelope: {"error":{"message":"..."}}
//  2. A Slack-style envelope: {"error":"some_code"}
//  3. A top-level {"message":"..."}
//  4. The raw body, trimmed and collapsed to a single line.
//
// Returns "" when the body is empty or contains only whitespace.
func extractServerMessage(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}

	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		// Nested object: {"error":{"message":"..."}}.
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
			return nested.Message
		}
		// Plain string: {"error":"some_code"}.
		var s string
		if json.Unmarshal(envelope.Error, &s) == nil && s != "" {
			return s
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}

	// Fall back to the raw body, collapsed to a single line so it's safe
	// to embed in an error message. Caps the length conservatively.
	const maxLen = 400
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > maxLen {
		text = text[:maxLen] + "\u2026"
	}
	return text
}

// getValidToken returns a non-expired token for the server, silently refreshing
// an expired token when a refresh token is available. Returns nil if no usable
// token can be obtained.
//
// If the stored token's recorded RequestedScopes no longer cover the scopes
// currently requested by the config, the stored token is discarded so that
// the next request triggers a fresh OAuth flow. This keeps us from reusing a
// token that was provisioned with too-narrow (or entirely wrong) scopes
// — typically after the user edits the scope list in their agent config.
func (t *oauthTransport) getValidToken(ctx context.Context) *OAuthToken {
	token, err := t.tokenStore.GetToken(t.baseURL)
	if err != nil {
		return nil
	}

	if !t.tokenCoversConfiguredScopes(token) {
		slog.DebugContext(ctx, "Stored token scopes no longer cover configured scopes; discarding to force re-auth",
			"url", t.baseURL,
			"stored", token.RequestedScopes,
			"configured", configuredScopes(t.oauthConfig),
		)
		if err := t.tokenStore.RemoveToken(t.baseURL); err != nil {
			slog.DebugContext(ctx, "Failed to remove stale token", "url", t.baseURL, "error", err)
		}
		return nil
	}

	if !token.IsExpired() {
		return token
	}

	if token.RefreshToken == "" {
		return nil
	}

	// Avoid hammering the token endpoint if a recent refresh already failed.
	const refreshBackoff = 30 * time.Second
	t.mu.Lock()
	failedAt := t.refreshFailedAt
	t.mu.Unlock()
	if !failedAt.IsZero() && time.Since(failedAt) < refreshBackoff {
		return nil
	}

	slog.DebugContext(ctx, "Attempting silent token refresh", "url", t.baseURL)

	o := &oauth{metadataClient: &http.Client{Timeout: 5 * time.Second}}
	authServer := cmp.Or(token.AuthServer, t.baseURL)
	metadata, err := o.getAuthorizationServerMetadata(ctx, authServer)
	if err != nil {
		slog.DebugContext(ctx, "Failed to fetch auth server metadata for refresh", "auth_server", authServer, "error", err)
		return nil
	}

	newToken, err := RefreshAccessToken(ctx, metadata.TokenEndpoint, token.RefreshToken, token.ClientID, token.ClientSecret)
	if err != nil {
		slog.DebugContext(ctx, "Token refresh failed, will require interactive auth", "error", err)
		t.mu.Lock()
		t.refreshFailedAt = time.Now()
		t.mu.Unlock()
		return nil
	}
	newToken.AuthServer = authServer
	newToken.RequestedScopes = token.RequestedScopes

	t.mu.Lock()
	t.refreshFailedAt = time.Time{} // reset on success
	t.mu.Unlock()

	if err := t.tokenStore.StoreToken(t.baseURL, newToken); err != nil {
		slog.WarnContext(ctx, "Failed to store refreshed token", "error", err)
	}

	slog.DebugContext(ctx, "Token refreshed successfully", "url", t.baseURL)
	return newToken
}

// tokenCoversConfiguredScopes reports whether the stored token was obtained
// with a scope set that still satisfies the config.
//
// Scoping rules (kept deliberately simple):
//   - If the config declares no scopes, any token is considered sufficient.
//   - If the stored token has no RequestedScopes (legacy tokens stored before
//     this field was introduced), it is treated as sufficient to avoid
//     forcing a re-auth on upgrade.
//   - Otherwise, every configured scope must appear in the token's
//     RequestedScopes.
func (t *oauthTransport) tokenCoversConfiguredScopes(token *OAuthToken) bool {
	configured := configuredScopes(t.oauthConfig)
	if len(configured) == 0 {
		return true
	}
	if len(token.RequestedScopes) == 0 {
		return true
	}
	stored := make(map[string]struct{}, len(token.RequestedScopes))
	for _, s := range token.RequestedScopes {
		stored[s] = struct{}{}
	}
	for _, s := range configured {
		if _, ok := stored[s]; !ok {
			return false
		}
	}
	return true
}

// configuredScopes is a nil-safe accessor for the Scopes slice on the
// optional RemoteOAuthConfig.
func configuredScopes(c *latest.RemoteOAuthConfig) []string {
	if c == nil {
		return nil
	}
	return c.Scopes
}

// handleOAuthFlow performs the OAuth flow when a 401 response is received
func (t *oauthTransport) handleOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	if t.managed {
		return t.handleManagedOAuthFlow(ctx, authServer, wwwAuth)
	}

	return t.handleUnmanagedOAuthFlow(ctx, authServer, wwwAuth)
}

func (t *oauthTransport) handleManagedOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	slog.DebugContext(ctx, "Starting OAuth flow for server", "url", t.baseURL)

	resourceURL := cmp.Or(resourceMetadataFromWWWAuth(wwwAuth), authServer+"/.well-known/oauth-protected-resource")

	resourceReq, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(resourceReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		_, _ = io.ReadAll(resp.Body)
		return errors.New("failed to fetch protected resource metadata")
	}
	var resourceMetadata protectedResourceMetadata
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&resourceMetadata); err != nil {
			return err
		}
	}

	if len(resourceMetadata.AuthorizationServers) == 0 {
		slog.DebugContext(ctx, "No authorization servers in resource metadata, using auth server from WWW-Authenticate header")
		resourceMetadata.AuthorizationServers = []string{authServer}
	}

	oauth := &oauth{metadataClient: &http.Client{Timeout: 5 * time.Second}}
	authServerMetadata, err := oauth.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	slog.DebugContext(ctx, "Creating OAuth callback server")
	var callbackPort int
	if t.oauthConfig != nil {
		callbackPort = t.oauthConfig.CallbackPort
	}
	callbackServer, err := NewCallbackServerOnPort(callbackPort)
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

	redirectURI := callbackServer.resolveRedirectURI(callbackRedirectURLFrom(t.oauthConfig))
	slog.DebugContext(ctx, "Using redirect URI", "uri", redirectURI)

	var clientID string
	var clientSecret string
	var scopes []string

	switch {
	case t.oauthConfig != nil && t.oauthConfig.ClientID != "":
		// Use explicit credentials from config
		slog.DebugContext(ctx, "Using explicit OAuth credentials from config")
		clientID = t.oauthConfig.ClientID
		clientSecret = t.oauthConfig.ClientSecret
		scopes = t.oauthConfig.Scopes
	case authServerMetadata.RegistrationEndpoint != "":
		slog.DebugContext(ctx, "Attempting dynamic client registration")
		clientID, clientSecret, err = RegisterClient(ctx, authServerMetadata, redirectURI, nil)
		if err != nil {
			slog.DebugContext(ctx, "Dynamic registration failed", "error", err)
			// TODO(rumpl): fall back to requesting client ID from user
			return err
		}
	default:
		// TODO(rumpl): fall back to requesting client ID from user
		return errors.New("authorization server does not support dynamic client registration and no explicit OAuth credentials configured")
	}

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
		t.baseURL,
		scopes,
	)

	result, err := t.client.requestElicitation(ctx, &mcpsdk.ElicitParams{
		Message:         fmt.Sprintf("The MCP server at %s requires OAuth authorization. Do you want to proceed?", t.baseURL),
		RequestedSchema: nil,
		Meta: map[string]any{
			"cagent/type":       "oauth_flow",
			"cagent/server_url": t.baseURL,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send elicitation request: %w", err)
	}

	slog.DebugContext(ctx, "Elicitation response received", "result", result)

	if result.Action != tools.ElicitationActionAccept {
		return errors.New("user declined OAuth authorization")
	}

	slog.DebugContext(ctx, "Requesting authorization code", "url", authURL)

	code, receivedState, err := RequestAuthorizationCode(ctx, authURL, callbackServer, state)
	if err != nil {
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		return errors.New("state mismatch in authorization response")
	}

	slog.DebugContext(ctx, "Exchanging authorization code for token")
	token, err := ExchangeCodeForToken(
		ctx,
		authServerMetadata.TokenEndpoint,
		code,
		verifier,
		clientID,
		clientSecret,
		redirectURI,
	)
	if err != nil {
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	token.ClientID = clientID
	token.ClientSecret = clientSecret
	token.AuthServer = resourceMetadata.AuthorizationServers[0]
	token.RequestedScopes = scopes

	if err := t.tokenStore.StoreToken(t.baseURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	// Notify the runtime that the OAuth flow was successful
	t.client.oauthSuccess()

	slog.DebugContext(ctx, "OAuth flow completed successfully")
	return nil
}

// handleUnmanagedOAuthFlow performs the OAuth flow for remote/unmanaged scenarios
// where the client handles the OAuth interaction instead of us
func (t *oauthTransport) handleUnmanagedOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	slog.DebugContext(ctx, "Starting unmanaged OAuth flow for server", "url", t.baseURL)

	// Extract resource URL from WWW-Authenticate header
	resourceURL := cmp.Or(resourceMetadataFromWWWAuth(wwwAuth), authServer+"/.well-known/oauth-protected-resource")

	resourceReq, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(resourceReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		_, _ = io.ReadAll(resp.Body)
		return errors.New("failed to fetch protected resource metadata")
	}
	var resourceMetadata protectedResourceMetadata
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&resourceMetadata); err != nil {
			return err
		}
	}

	if len(resourceMetadata.AuthorizationServers) == 0 {
		slog.DebugContext(ctx, "No authorization servers in resource metadata, using auth server from WWW-Authenticate header")
		resourceMetadata.AuthorizationServers = []string{authServer}
	}

	oauth := &oauth{metadataClient: &http.Client{Timeout: 5 * time.Second}}
	authServerMetadata, err := oauth.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	slog.DebugContext(ctx, "Sending OAuth elicitation request to client")

	result, err := t.client.requestElicitation(ctx, &mcpsdk.ElicitParams{
		Message:         "OAuth authorization required for " + t.baseURL,
		RequestedSchema: nil,
		Meta: map[string]any{
			"cagent/type":          "oauth_flow",
			"cagent/server_url":    t.baseURL,
			"auth_server":          resourceMetadata.AuthorizationServers[0],
			"auth_server_metadata": authServerMetadata,
			"resource_metadata":    resourceMetadata,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send elicitation request: %w", err)
	}

	slog.DebugContext(ctx, "Received elicitation response from client", "action", result.Action)

	if result.Action != tools.ElicitationActionAccept {
		return errors.New("OAuth flow declined or cancelled by client")
	}
	if result.Content == nil {
		return errors.New("no token received from client")
	}

	tokenData := result.Content

	token := &OAuthToken{}

	if accessToken, ok := tokenData["access_token"].(string); ok {
		token.AccessToken = accessToken
	} else {
		return errors.New("access_token missing or invalid in client response")
	}

	if tokenType, ok := tokenData["token_type"].(string); ok {
		token.TokenType = tokenType
	}

	if expiresIn, ok := tokenData["expires_in"].(float64); ok {
		token.ExpiresIn = int(expiresIn)
		token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	token.AuthServer = resourceMetadata.AuthorizationServers[0]

	if refreshToken, ok := tokenData["refresh_token"].(string); ok {
		token.RefreshToken = refreshToken
	}
	if t.oauthConfig != nil {
		token.RequestedScopes = t.oauthConfig.Scopes
	}
	if err := t.tokenStore.StoreToken(t.baseURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	// Notify the runtime that the OAuth flow was successful
	t.client.oauthSuccess()

	slog.DebugContext(ctx, "Managed OAuth flow completed successfully")
	return nil
}
