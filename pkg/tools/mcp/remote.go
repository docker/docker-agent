package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/upstream"
)

type remoteMCPClient struct {
	sessionClient

	url                       string
	transportType             string
	headers                   map[string]string
	tokenStore                OAuthTokenStore
	managed                   bool
	unmanagedOAuthRedirectURI string
	oauthConfig               *latest.RemoteOAuthConfig
	allowPrivateIPs           bool
}

func newRemoteClient(
	url, transportType string,
	headers map[string]string,
	tokenStore OAuthTokenStore,
	oauthConfig *latest.RemoteOAuthConfig,
	allowPrivateIPs bool,
) *remoteMCPClient {
	slog.Debug("Creating remote MCP client",
		"url", url,
		"transport", transportType,
		"headers", headers,
		"allow_private_ips", allowPrivateIPs,
	)

	if tokenStore == nil {
		tokenStore = NewInMemoryTokenStore()
	}

	return &remoteMCPClient{
		sessionClient:   sessionClient{serverAddress: sanitizeRemoteAddress(url)},
		url:             url,
		transportType:   transportType,
		headers:         headers,
		tokenStore:      tokenStore,
		oauthConfig:     oauthConfig,
		allowPrivateIPs: allowPrivateIPs,
	}
}

// sanitizeRemoteAddress extracts a span-safe identifier from an MCP URL
// before stamping it as `server.address`. The URL may legitimately
// contain credentials in userinfo (`https://user:token@host/`) or query
// params (`?api_key=...`); sending those to the trace backend would be
// a real exfiltration risk. OTel's semantic convention for
// `server.address` is the host (with optional port) anyway, so we keep
// only `u.Host` and drop everything else.
//
// Returns the empty string on parse failure or hostless URLs (file://,
// stdio commands, malformed input). The caller stamps `server.address`
// only when it's non-empty, so a sanitisation miss leaves the span
// without that attribute rather than leaking a raw URL.
func sanitizeRemoteAddress(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

func (c *remoteMCPClient) Initialize(ctx context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	// Create HTTP client with OAuth support. We keep a reference to the
	// oauthTransport so we can enrich Connect errors with the server's own
	// explanation — without this, a plain `Bad Request` bubbles up and the
	// user has no idea that, say, the Slack app hasn't been enabled for MCP.
	httpClient, oauthT, err := c.createHTTPClient()
	if err != nil {
		return nil, fmt.Errorf("creating HTTP client: %w", err)
	}

	var transport gomcp.Transport

	switch c.transportType {
	case "sse":
		transport = &gomcp.SSEClientTransport{
			Endpoint:   c.url,
			HTTPClient: httpClient,
		}
	case "streamable", "streamable-http":
		transport = &gomcp.StreamableClientTransport{
			Endpoint:             c.url,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true,
		}
	default:
		return nil, fmt.Errorf("unsupported transport type: %s", c.transportType)
	}

	// Create an MCP client with elicitation support
	impl := &gomcp.Implementation{
		Name:    "docker agent",
		Version: "1.0.0",
	}

	toolChanged, promptChanged := c.notificationHandlers()

	opts := &gomcp.ClientOptions{
		ElicitationHandler:       c.handleElicitationRequest,
		CreateMessageHandler:     c.handleSamplingRequest,
		ToolListChangedHandler:   toolChanged,
		PromptListChangedHandler: promptChanged,
	}

	client := gomcp.NewClient(impl, opts)

	// Connect to the MCP server
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, enrichConnectError(err, oauthT)
	}

	c.setSession(session)

	slog.DebugContext(ctx, "Remote MCP client connected successfully")
	return session.InitializeResult(), nil
}

// enrichConnectError wraps the error returned by client.Connect with any
// server-side failure message captured by the transport. The MCP SDK
// surfaces only http.StatusText ("Bad Request", "Forbidden", ...) even when
// the server included a useful JSON-RPC error payload, so we append the
// extracted message here so callers — and ultimately the user — can see it.
//
// It also recognises the deferred-OAuth case (the transport returned an
// AuthorizationRequiredError because the request context disallowed prompts)
// and re-emits a clean AuthorizationRequiredError so callers can distinguish
// it from a real failure with errors.As. We can't rely on the SDK's own
// wrapping for this because the SDK uses fmt.Errorf("%w: %v", …) when it
// surfaces transport errors — the original error is included as text only,
// not in the unwrap chain.
//
// Pre: err != nil and t != nil; only called from the Connect failure path.
func enrichConnectError(err error, t *oauthTransport) error {
	// Order matters: a decline implies the interactive OAuth flow
	// actually ran, so lastOAuthDeclined wins over lastAuthRequired in
	// the (in practice impossible) case that both flags are set.
	if t.oauthDeclined() {
		return &OAuthDeclinedError{URL: t.baseURL}
	}
	if t.authorizationRequired() {
		return &AuthorizationRequiredError{URL: t.baseURL}
	}
	if status, msg := t.lastServerError(); status != 0 && msg != "" {
		return fmt.Errorf("failed to connect to MCP server: %w (server responded %d: %s)", err, status, msg)
	}
	return fmt.Errorf("failed to connect to MCP server: %w", err)
}

// SetManagedOAuth sets whether OAuth should be handled in managed mode.
// In managed mode, the client handles the OAuth flow instead of the server.
func (c *remoteMCPClient) SetManagedOAuth(managed bool) {
	c.mu.Lock()
	c.managed = managed
	c.mu.Unlock()
}

// SetUnmanagedOAuthRedirectURI sets the redirect URI docker-agent advertises
// when running the OAuth flow in unmanaged mode. See OAuthCapable for full
// semantics.
func (c *remoteMCPClient) SetUnmanagedOAuthRedirectURI(uri string) {
	c.mu.Lock()
	c.unmanagedOAuthRedirectURI = uri
	c.mu.Unlock()
}

// createHTTPClient creates an HTTP client with custom headers and OAuth support.
// Header values may contain ${headers.NAME} placeholders that are resolved
// at request time from upstream headers stored in the request context.
//
// The oauthTransport is returned alongside the client so callers can inspect
// the most recent server-side failure (via lastServerError) when Connect()
// returns a bare HTTP-status error and we need to surface the actual cause.
//
// The transport chain wraps `httpclient.WrapWithOTel` outermost so every
// outbound MCP request injects W3C `traceparent` (and creates an HTTP
// CLIENT span). Without this wrap, the streamable-HTTP / SSE transports
// the gomcp SDK builds with our `*http.Client` send raw POST/GET requests
// that never chain onto the calling cagent span — the downstream MCP
// server's spans then live in a separate root trace, breaking end-to-end
// observability for any agent talking to a remote MCP. `WrapWithOTel` is
// a no-op when OTel is disabled at runtime, so the laptop-mode default
// stays unchanged.
func (c *remoteMCPClient) createHTTPClient() (*http.Client, *oauthTransport, error) {
	base := c.headerTransport()

	// Then wrap with OAuth support
	oauthT := &oauthTransport{
		base:                      base,
		client:                    c,
		tokenStore:                c.tokenStore,
		baseURL:                   c.url,
		managed:                   c.managed,
		unmanagedOAuthRedirectURI: c.unmanagedOAuthRedirectURI,
		oauthConfig:               c.oauthConfig,
		oauthHTTPClient:           oauthHTTPClientWithHeaders(c.url, c.headers, c.allowPrivateIPs),
	}

	// Persist cookies across requests
	// So sticky sessions work if implemented by the server (e.g. in a multiple replica setup)
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("creating cookie jar: %w", err)
	}
	return &http.Client{Transport: httpclient.WrapWithOTel(oauthT), Jar: jar}, oauthT, nil
}

func (c *remoteMCPClient) headerTransport() http.RoundTripper {
	if len(c.headers) > 0 {
		return upstream.NewHeaderTransport(http.DefaultTransport, c.headers)
	}
	return http.DefaultTransport
}
