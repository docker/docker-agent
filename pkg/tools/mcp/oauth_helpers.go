package mcp

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
	"github.com/docker/docker-agent/pkg/upstream"
)

// The OAuth authorization-code flow (state/PKCE generation, callback server,
// token exchange/refresh, dynamic client registration) lives in the
// oauthflow subpackage so packages that only drive the flow don't link the
// whole MCP toolset. Forwarders expose the scope-owned flow here too; the
// header-scoping transport depends on the MCP upstream header resolver.

// OAuthToken is an alias for [oauthflow.OAuthToken].
type OAuthToken = oauthflow.OAuthToken

// AuthorizationServerMetadata is an alias for [oauthflow.AuthorizationServerMetadata].
type AuthorizationServerMetadata = oauthflow.AuthorizationServerMetadata

// CallbackAddress is immutable callback endpoint information.
type CallbackAddress = oauthflow.CallbackAddress

// AuthorizationRequest describes the URL and state for one authorization.
type AuthorizationRequest = oauthflow.AuthorizationRequest

// PrepareAuthorization prepares authorization after the callback port is bound.
type PrepareAuthorization = oauthflow.PrepareAuthorization

// GenerateState generates a random state parameter for OAuth CSRF protection
func GenerateState() (string, error) {
	return oauthflow.GenerateState()
}

// GeneratePKCEVerifier generates a PKCE code verifier using oauth2 library
func GeneratePKCEVerifier() string {
	return oauthflow.GeneratePKCEVerifier()
}

// BuildAuthorizationURL builds the OAuth authorization URL with PKCE.
func BuildAuthorizationURL(authEndpoint, clientID, redirectURI, state, codeChallenge, resourceURL string, scopes []string) string {
	return oauthflow.BuildAuthorizationURL(authEndpoint, clientID, redirectURI, state, codeChallenge, resourceURL, scopes)
}

// RequestAuthorizationCode owns the callback listener for one authorization.
func RequestAuthorizationCode(ctx context.Context, port int, prepare PrepareAuthorization) (string, string, error) {
	return oauthflow.RequestAuthorizationCode(ctx, port, prepare)
}

// ExchangeCodeForToken exchanges an authorization code for an access token.
func ExchangeCodeForToken(ctx context.Context, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI string) (*OAuthToken, error) {
	return oauthflow.ExchangeCodeForToken(ctx, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI)
}

// ExchangeCodeForTokenWithResource exchanges an authorization code and sends
// the RFC 8707 resource indicator to token endpoints that require it.
func ExchangeCodeForTokenWithResource(ctx context.Context, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI, resourceURL string) (*OAuthToken, error) {
	return oauthflow.ExchangeCodeForTokenWithResource(ctx, tokenEndpoint, code, codeVerifier, clientID, clientSecret, redirectURI, resourceURL)
}

// RegisterClient performs dynamic client registration
func RegisterClient(ctx context.Context, authMetadata *AuthorizationServerMetadata, redirectURI string, scopes []string) (clientID, clientSecret, authMethod string, err error) {
	return oauthflow.RegisterClient(ctx, authMetadata, redirectURI, scopes)
}

// RefreshAccessToken uses a refresh token to obtain a new access token
// without user interaction.
func RefreshAccessToken(ctx context.Context, tokenEndpoint, refreshToken, clientID, clientSecret string) (*OAuthToken, error) {
	return oauthflow.RefreshAccessToken(ctx, tokenEndpoint, refreshToken, clientID, clientSecret)
}

func oauthHTTPClientForAllowPrivateIPs(allowPrivateIPs bool) *http.Client {
	return oauthflow.HTTPClientForAllowPrivateIPs(allowPrivateIPs)
}

// oauthHTTPClientWithHeaders builds the HTTP client used by the OAuth flow
// (protected-resource / authorization-server metadata discovery, dynamic
// client registration, token exchange and refresh). It layers the configured
// custom headers on top of the SSRF-safe (or allow_private_ips) transport,
// but ONLY for requests that target the MCP server's own host. Header values
// are resolved per request through resolve (nil means ${headers.NAME}-only
// resolution), so env-backed values stay as fresh here as on the main channel.
//
// OAuth metadata can advertise authorization servers on hosts chosen by the
// (untrusted) server response, so forwarding the configured headers to every
// OAuth request would leak credentials (Authorization, API keys, ...) meant
// for the MCP server to a third party. Scoping to the server's host mirrors
// the main channel, which only ever talks to rawURL.
//
// This is what makes routing headers such as Grafana Cloud's X-Grafana-URL
// reach the protected-resource-metadata request (served by the MCP host
// itself), so the OAuth flow is scoped to the right instance instead of
// prompting the user for it. See issue #3148.
//
// The returned client is a fresh instance; the shared SSRF-safe client
// singleton is never mutated.
func oauthHTTPClientWithHeaders(rawURL string, headers map[string]string, allowPrivateIPs bool, resolve upstream.HeaderResolver) *http.Client {
	base := oauthHTTPClientForAllowPrivateIPs(allowPrivateIPs)
	if len(headers) == 0 {
		return base
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return base
	}

	// nil Transport means net/http uses DefaultTransport; make it explicit so
	// the header wrapper sits outside it and the underlying (SSRF-safe) dialer
	// still runs for header and non-header requests alike.
	inner := base.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &http.Client{
		Timeout:       base.Timeout,
		CheckRedirect: base.CheckRedirect,
		Transport: &hostScopedHeaderTransport{
			host:        hostWithoutDefaultPort(u.Host, u.Scheme),
			withHeaders: upstream.NewHeaderTransportWithResolverForOrigin(inner, rawURL, headers, resolve),
			base:        inner,
		},
	}
}

// hostScopedHeaderTransport applies the configured custom headers only to
// requests whose host matches host; every other request (e.g. an OAuth call
// to a third-party authorization server advertised in server metadata) goes
// through base unchanged so configured credentials are never forwarded
// off-host.
type hostScopedHeaderTransport struct {
	host        string
	withHeaders http.RoundTripper
	base        http.RoundTripper
}

func (t *hostScopedHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(hostWithoutDefaultPort(req.URL.Host, req.URL.Scheme), t.host) {
		return t.withHeaders.RoundTrip(req)
	}
	return t.base.RoundTrip(req)
}

// hostWithoutDefaultPort strips the scheme's default port from host so that
// "mcp.example.com:443" and "mcp.example.com" compare equal under https
// (likewise :80 under http). A non-default or absent port is left untouched.
//
// Both sides of the host-scoping comparison are normalised through this so
// headers still flow when the configured URL and a server-advertised
// discovery URL disagree on whether to spell out the standard port.
func hostWithoutDefaultPort(host, scheme string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host // no port present
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return h
	}
	return host
}
