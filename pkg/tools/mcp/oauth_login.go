package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
)

// oauthLoginHTTPClient builds the SSRF-safe *http.Client used for every
// request PerformOAuthLogin makes (probe, protected-resource and
// authorization-server metadata discovery, DCR, and token exchange). Tests
// replace it via setOAuthLoginHTTPClientForTesting to reach httptest
// servers, which bind to 127.0.0.1 — an address this client refuses to dial
// by design.
var oauthLoginHTTPClient = func() *http.Client {
	return httpclient.NewSafeClient(5*time.Second, false)
}

// setOAuthLoginHTTPClientForTesting replaces the client PerformOAuthLogin
// uses and returns a function restoring the previous one. Never call this
// outside tests.
func setOAuthLoginHTTPClientForTesting(c *http.Client) (restore func()) {
	prev := oauthLoginHTTPClient
	oauthLoginHTTPClient = func() *http.Client { return c }
	return func() { oauthLoginHTTPClient = prev }
}

// PerformOAuthLogin performs a standalone OAuth flow for the given remote
// MCP server. It probes the server unauthenticated, discovers protected-
// resource and authorization-server metadata, resolves an OAuth client
// (explicit config or Dynamic Client Registration), opens the browser for
// user authorization, and stores the resulting token in the keyring.
//
// remote.URL is used verbatim as the probe target, the RFC 8707 resource
// indicator for both /authorize and the token exchange, and the
// token-store key — even when protected-resource metadata reports a
// different resource — so this command and `debug oauth remove` operate on
// the exact same token the runtime would use for the same configured
// remote.
//
// Discovery mirrors the runtime OAuth flows in oauth.go as closely as a
// non-interactive CLI command can: when the unauthenticated probe's
// challenge names an exact resource_metadata URL (as opposed to a bare
// resource identifier, which is not treated as a metadata URL), that URL is
// authoritative and no other candidate is tried — not even on a 404, which
// is a hard error for that exact candidate. Otherwise the RFC 9728 §3.1
// path-insertion URL is tried first and the origin-root well-known URL only
// after that 404s. Any decode failure or non-404/non-200 status on an
// attempted candidate is a hard error: no later candidate, DCR, browser, or
// token request follows.
//
// The callback/redirect mechanics below (NewCallbackServer, GetRedirectURI)
// are deliberately left unchanged: aligning them with the runtime's
// NewCallbackServerOnPort/ResolveRedirectURI (and honoring
// RemoteOAuthConfig.CallbackPort/CallbackRedirectURL here) is a separate,
// not-yet-made decision.
func PerformOAuthLogin(ctx context.Context, remote latest.Remote) error {
	tokenStore := NewKeyringTokenStore()
	client := oauthLoginHTTPClient()

	serverURL := remote.URL
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	authOrigin := parsed.Scheme + "://" + parsed.Host

	wwwAuth, err := probeUnauthenticated(ctx, client, serverURL)
	if err != nil {
		return err
	}

	resourceURL, opts := protectedResourceMetadataCandidates(parsed, authOrigin, wwwAuth)
	resourceMetadata, err := fetchProtectedResourceMetadata(ctx, client, resourceURL, authOrigin, opts)
	if err != nil {
		return fmt.Errorf("protected resource metadata discovery for %s failed: %w", sanitizeURLForLog(resourceURL), err)
	}

	o := &oauth{metadataClient: client}
	authServerMetadata, err := o.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	// Set up the callback server for the redirect.
	callbackServer, err := NewCallbackServer(ctx)
	if err != nil {
		return fmt.Errorf("failed to create callback server: %w", err)
	}
	defer func() {
		// Detach from ctx's cancellation (the request may be done) but
		// keep its trace context for the shutdown.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Failed to shutdown callback server", "error", err)
		}
	}()

	if err := callbackServer.Start(); err != nil {
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.GetRedirectURI()

	clientID, clientSecret, scopes, err := resolveStandaloneClientCredentials(
		ctx, client, remote.OAuth, authServerMetadata, redirectURI,
		challengeScopesFromWWWAuth(wwwAuth), resourceMetadata.ScopesSupported,
	)
	if err != nil {
		return err
	}

	// Generate PKCE and state.
	state, err := GenerateState()
	if err != nil {
		return fmt.Errorf("failed to generate state: %w", err)
	}
	callbackServer.SetExpectedState(state)
	verifier := GeneratePKCEVerifier()
	// remote.URL verbatim, never resourceMetadata.Resource: the resource
	// indicator must match the token-store key exactly (see the
	// PerformOAuthLogin doc comment above).
	resourceIndicator := serverURL

	authURL := BuildAuthorizationURL(
		authServerMetadata.AuthorizationEndpoint,
		clientID,
		redirectURI,
		state,
		oauth2.S256ChallengeFromVerifier(verifier),
		resourceIndicator,
		scopes,
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
	token, err := oauthflow.ExchangeCodeForTokenWithClient(ctx, client, authServerMetadata.TokenEndpoint, code, verifier, clientID, clientSecret, redirectURI, resourceIndicator)
	if err != nil {
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	token.ClientID = clientID
	token.ClientSecret = clientSecret
	token.AuthServer = resourceMetadata.AuthorizationServers[0]
	token.RequestedScopes = scopes

	if err := tokenStore.StoreToken(serverURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	return nil
}

// probeUnauthenticated issues a single unauthenticated GET request to
// serverURL and returns the response's WWW-Authenticate header, if any.
// RFC 9728 §5.1 describes a Bearer challenge carrying resource_metadata for
// the server's exact protected-resource metadata URL as how a protected MCP
// server advertises OAuth support — it is not guaranteed: a server may
// respond without that challenge (wrong method, no auth required, or a
// transport that only challenges on a real MCP request). That case yields
// an empty string here, and discovery falls back to the RFC 9728
// path-insertion/origin-root candidates instead of erroring outright.
func probeUnauthenticated(ctx context.Context, client *http.Client, serverURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("failed to create probe request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to probe %s: %w", sanitizeURLForLog(serverURL), err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.Header.Get("WWW-Authenticate"), nil
}

// protectedResourceMetadataCandidates decides the primary protected-resource
// metadata URL and any opt-in fallback candidates for fetchProtectedResourceMetadata.
//
// It consults only the challenge's resource_metadata auth-param, never the
// bare resource auth-param (which may name the protected resource itself,
// not a metadata document, and could point the discovery GET/JSON-decode at
// the MCP endpoint). When resource_metadata names an exact URL, that URL is
// authoritative: it is returned as the sole candidate, with no fallback and
// NotFoundIsHardError set, so a 404/error on it hard-stops discovery instead
// of being papered over by guessing another URL. Otherwise the RFC 9728
// §3.1 path-insertion URL for serverURL is tried first, with the
// origin-root well-known URL as the one fallback candidate tried only if
// the path-insertion URL 404s.
func protectedResourceMetadataCandidates(serverURL *url.URL, authOrigin, wwwAuth string) (primary string, opts protectedResourceMetadataOptions) {
	if challenged := parseAuthParams(wwwAuth)["resource_metadata"]; challenged != "" {
		return challenged, protectedResourceMetadataOptions{NotFoundIsHardError: true}
	}

	opts.FallbackCandidateURLs = []string{authOrigin + "/.well-known/oauth-protected-resource"}
	return protectedResourceMetadataPathInsertionURL(serverURL), opts
}

// protectedResourceMetadataPathInsertionURL returns the RFC 9728 §3.1
// path-aware protected-resource metadata URL for resourceURL: the
// well-known suffix is inserted between origin and path, e.g.
// https://mcp.atlassian.com/v1/mcp/authv2 becomes
// https://mcp.atlassian.com/.well-known/oauth-protected-resource/v1/mcp/authv2.
func protectedResourceMetadataPathInsertionURL(resourceURL *url.URL) string {
	origin := resourceURL.Scheme + "://" + resourceURL.Host
	path := strings.TrimSuffix(resourceURL.Path, "/")
	return origin + "/.well-known/oauth-protected-resource" + path
}

// resolveStandaloneClientCredentials picks the OAuth client_id (and optional
// secret + scopes) for the standalone CLI login. It mirrors
// oauthTransport.resolveClientCredentials' explicit-credentials/DCR selector,
// but this is a non-interactive CLI command: unlike the runtime flows, a
// missing or failing Dynamic Client Registration is a hard error rather than
// falling back to an interactive credentials prompt.
//
// challengeScopes and prmScopesSupported feed selectDCRScopes exactly as
// they do at runtime: on a successful DCR the returned scopes are the exact
// scopes requested at registration and must be reused for /authorize and
// the stored token's RequestedScopes.
func resolveStandaloneClientCredentials(
	ctx context.Context,
	client *http.Client,
	oauthConfig *latest.RemoteOAuthConfig,
	authServerMetadata *AuthorizationServerMetadata,
	redirectURI string,
	challengeScopes, prmScopesSupported []string,
) (clientID, clientSecret string, scopes []string, err error) {
	if oauthConfig != nil && oauthConfig.ClientID != "" {
		slog.DebugContext(ctx, "Using explicit OAuth credentials from config")
		return oauthConfig.ClientID, oauthConfig.ClientSecret, oauthConfig.Scopes, nil
	}

	if authServerMetadata.RegistrationEndpoint == "" {
		return "", "", nil, errors.New("authorization server does not support dynamic client registration; configure clientId (and clientSecret, if required) to use a pre-registered client")
	}

	// Select before registering so the exact same scopes go into the DCR
	// request and are returned for reuse at /authorize and on the stored
	// token.
	requestedScopes := selectDCRScopes(configuredScopes(oauthConfig), challengeScopes, prmScopesSupported)
	clientID, clientSecret, err = oauthflow.RegisterClientWithClient(ctx, client, authServerMetadata, redirectURI, requestedScopes)
	if err != nil {
		return "", "", nil, fmt.Errorf("dynamic client registration failed: %w", err)
	}
	return clientID, clientSecret, requestedScopes, nil
}
