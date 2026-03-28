package mcp

import (
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
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, openIDURL, http.NoBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")

		resp, err := o.metadataClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			// Return default metadata if all discovery fails
			return createDefaultMetadata(authServerURL), nil
		}
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
	metadata.RegistrationEndpoint = cmp.Or(metadata.RegistrationEndpoint, authServerURL+"/register")

	return metadata
}

// createDefaultMetadata creates minimal metadata when discovery fails
func createDefaultMetadata(authServerURL string) *AuthorizationServerMetadata {
	return &AuthorizationServerMetadata{
		Issuer:                                 authServerURL,
		AuthorizationEndpoint:                  authServerURL + "/authorize",
		TokenEndpoint:                          authServerURL + "/token",
		RegistrationEndpoint:                   authServerURL + "/register",
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

// oauthTransport wraps an HTTP transport with OAuth support
type oauthTransport struct {
	base http.RoundTripper
	// TODO(rumpl): remove client reference, we need to find a better way to send elicitation requests
	client      *remoteMCPClient
	tokenStore  OAuthTokenStore
	baseURL     string
	managed     bool
	oauthConfig *RemoteOAuthConfig
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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

	if token, err := t.tokenStore.GetToken(t.baseURL); err == nil && !token.IsExpired() {
		reqClone.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}

	resp, err := t.base.RoundTrip(reqClone)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if wwwAuth != "" {
			resp.Body.Close()

			authServer := req.URL.Scheme + "://" + req.URL.Host
			if err := t.handleOAuthFlow(req.Context(), authServer, wwwAuth); err != nil {
				return nil, fmt.Errorf("OAuth flow failed: %w", err)
			}

			if len(bodyBytes) > 0 {
				req.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			}

			return t.RoundTrip(req)
		}
	}

	return resp, nil
}

// handleOAuthFlow performs the OAuth flow when a 401 response is received
func (t *oauthTransport) handleOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	if t.managed {
		return t.handleManagedOAuthFlow(ctx, authServer, wwwAuth)
	}

	return t.handleUnmanagedOAuthFlow(ctx, authServer, wwwAuth)
}

func (t *oauthTransport) handleManagedOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	slog.Debug("Starting OAuth flow for server", "url", t.baseURL)

	resourceURL := cmp.Or(resourceMetadataFromWWWAuth(wwwAuth), authServer+"/.well-known/oauth-protected-resource")

	resp, err := http.Get(resourceURL)
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
		slog.Debug("No authorization servers in resource metadata, using auth server from WWW-Authenticate header")
		resourceMetadata.AuthorizationServers = []string{authServer}
	}

	oauth := &oauth{metadataClient: &http.Client{Timeout: 5 * time.Second}}
	authServerMetadata, err := oauth.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	slog.Debug("Creating OAuth callback server")
	callbackServer, err := NewCallbackServer()
	if err != nil {
		return fmt.Errorf("failed to create callback server: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("Failed to shutdown callback server", "error", err)
		}
	}()

	// Use explicit callback port if configured
	if t.oauthConfig != nil && t.oauthConfig.CallbackPort > 0 {
		callbackServer.SetPort(t.oauthConfig.CallbackPort)
	}

	if err := callbackServer.Start(); err != nil {
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.GetRedirectURI()
	slog.Debug("Using redirect URI", "uri", redirectURI)

	var clientID string
	var clientSecret string

	// Use explicit OAuth credentials if provided, otherwise attempt dynamic registration
	if t.oauthConfig != nil && t.oauthConfig.ClientID != "" {
		slog.Debug("Using explicit OAuth client ID from configuration")
		clientID = t.oauthConfig.ClientID
		clientSecret = t.oauthConfig.ClientSecret
	} else if authServerMetadata.RegistrationEndpoint != "" {
		slog.Debug("Attempting dynamic client registration")
		clientID, clientSecret, err = RegisterClient(ctx, authServerMetadata, redirectURI, t.oauthConfig.Scopes)
		if err != nil {
			slog.Debug("Dynamic registration failed", "error", err)
			// If explicit client ID was not provided and registration failed, return error
			return fmt.Errorf("dynamic client registration failed and no explicit client ID provided: %w", err)
		}
	} else {
		// No dynamic registration support and no explicit credentials
		return errors.New("authorization server does not support dynamic client registration and no explicit OAuth credentials provided")
	}

	state, err := GenerateState()
	if err != nil {
		return fmt.Errorf("failed to generate state: %w", err)
	}

	callbackServer.SetExpectedState(state)
	verifier := GeneratePKCEVerifier()

	// Use explicit scopes if provided, otherwise use server defaults
	scopes := authServerMetadata.ScopesSupported
	if t.oauthConfig != nil && len(t.oauthConfig.Scopes) > 0 {
		scopes = t.oauthConfig.Scopes
	}

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

	slog.Debug("Elicitation response received", "result", result)

	if result.Action != tools.ElicitationActionAccept {
		return errors.New("user declined OAuth authorization")
	}

	slog.Debug("Requesting authorization code", "url", authURL)

	code, receivedState, err := RequestAuthorizationCode(ctx, authURL, callbackServer, state)
	if err != nil {
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		return errors.New("state mismatch in authorization response")
	}

	slog.Debug("Exchanging authorization code for token")
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

	if err := t.tokenStore.StoreToken(t.baseURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	// Notify the runtime that the OAuth flow was successful
	t.client.oauthSuccess()

	slog.Debug("OAuth flow completed successfully")
	return nil
}

// handleUnmanagedOAuthFlow performs the OAuth flow for remote/unmanaged scenarios
// where the client handles the OAuth interaction instead of us
func (t *oauthTransport) handleUnmanagedOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	slog.Debug("Starting unmanaged OAuth flow for server", "url", t.baseURL)

	// Extract resource URL from WWW-Authenticate header
	resourceURL := cmp.Or(resourceMetadataFromWWWAuth(wwwAuth), authServer+"/.well-known/oauth-protected-resource")

	resp, err := http.Get(resourceURL)
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
		slog.Debug("No authorization servers in resource metadata, using auth server from WWW-Authenticate header")
		resourceMetadata.AuthorizationServers = []string{authServer}
	}

	oauth := &oauth{metadataClient: &http.Client{Timeout: 5 * time.Second}}
	authServerMetadata, err := oauth.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	slog.Debug("Sending OAuth elicitation request to client")

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

	slog.Debug("Received elicitation response from client", "action", result.Action)

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

	if refreshToken, ok := tokenData["refresh_token"].(string); ok {
		token.RefreshToken = refreshToken
	}
	if err := t.tokenStore.StoreToken(t.baseURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	// Notify the runtime that the OAuth flow was successful
	t.client.oauthSuccess()

	slog.Debug("Managed OAuth flow completed successfully")
	return nil
}
