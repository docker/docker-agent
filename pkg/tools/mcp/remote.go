package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/upstream"
)

type remoteMCPClient struct {
	sessionClient

	url                       string
	transportType             string
	headers                   map[string]string
	envExpander               *js.Expander // nil when no env provider was supplied
	tokenStore                OAuthTokenStore
	managed                   bool
	unmanagedOAuthRedirectURI string
	oauthConfig               *latest.RemoteOAuthConfig
	allowPrivateIPs           bool
	// standaloneSSE opts in to the streamable transport's standalone SSE GET
	// stream. Off by default: some remote servers hang when the client opens
	// the persistent stream (see commit 332f5928, go-sdk issue #633). Servers
	// that send server-initiated requests outside of client calls, most
	// importantly keepalive pings (e.g. some MCP gateways send keepalive pings
	// and close the session when the ping cannot be delivered), require
	// it, so such toolsets opt in via Toolset.SetStandaloneSSE(true).
	standaloneSSE bool
}

func newRemoteClient(
	url, transportType string,
	headers map[string]string,
	tokenStore OAuthTokenStore,
	oauthConfig *latest.RemoteOAuthConfig,
	allowPrivateIPs bool,
	env environment.Provider,
) *remoteMCPClient {
	slog.Debug("Creating remote MCP client",
		"server", sanitizeRemoteAddress(url),
		"transport", transportType,
		"header_names", headerNames(headers),
		"allow_private_ips", allowPrivateIPs,
	)

	if tokenStore == nil {
		tokenStore = NewInMemoryTokenStore()
	}

	var envExpander *js.Expander
	if env != nil {
		envExpander = js.NewJsExpander(env)
	}

	return &remoteMCPClient{
		sessionClient:   sessionClient{serverAddress: sanitizeRemoteAddress(url)},
		url:             url,
		transportType:   transportType,
		headers:         headers,
		envExpander:     envExpander,
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

func sanitizeURLForLog(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// unixSocketPath mirrors pkg/server.Listen: it strips the "unix://" prefix so
// `unix:///tmp/x.sock` maps to `/tmp/x.sock`.
func unixSocketPath(rawURL string) (string, bool) {
	return strings.CutPrefix(rawURL, "unix://")
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

	endpoint := c.url
	if _, ok := unixSocketPath(c.url); ok {
		// http.Client can't dial the "unix" scheme; the socket is dialed by
		// the transport, the host here is a placeholder.
		endpoint = "http://localhost/"
	}

	switch c.transportType {
	case "sse":
		transport = &gomcp.SSEClientTransport{
			Endpoint:   endpoint,
			HTTPClient: httpClient,
		}
	case "streamable", "streamable-http":
		c.mu.RLock()
		standaloneSSE := c.standaloneSSE
		c.mu.RUnlock()
		transport = &gomcp.StreamableClientTransport{
			Endpoint:             endpoint,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: !standaloneSSE,
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

	// Sampling registration is delegated to applySamplingHandlerOpts so the
	// with-tools callback is wired eagerly even when handler fields are still
	// nil at Initialize time — see that method for the ordering rationale.
	opts := &gomcp.ClientOptions{
		ElicitationHandler:       c.handleElicitationRequest,
		ToolListChangedHandler:   toolChanged,
		PromptListChangedHandler: promptChanged,
	}
	c.applySamplingHandlerOpts(opts)

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
	// Wrap on status alone: many rate-limit / load-balancer 429s and 503s
	// carry an empty body, so gating on msg != "" (as an earlier version of
	// this code did) silently dropped the *modelerrors.StatusError wrap —
	// and with it, the StartableToolSet backoff gate never armed.
	//
	// status, msg and retryAfter are read together as a single snapshot
	// (rather than via two separately-locked accessor calls) so they can
	// never be pieced together from two different concurrent responses on
	// this transport (e.g. a standalone SSE probe racing the initialize call).
	if status, msg, retryAfter := t.lastServerErrorSnapshot(); status != 0 {
		var enriched error
		if msg != "" {
			enriched = fmt.Errorf("failed to connect to MCP server: %w (server responded %d: %s)", err, status, msg)
		} else {
			// No status text extracted from the body: modelerrors.StatusError.Error()
			// already prefixes "HTTP %d: ", so repeating the code here would read as
			// "HTTP 503: ... (server responded 503)".
			enriched = fmt.Errorf("failed to connect to MCP server: %w", err)
		}
		// Forward the server's Retry-After hint (if any) so the backoff gate
		// honors it instead of falling back to the generic computed delay.
		resp := &http.Response{Header: http.Header{}}
		if retryAfter != "" {
			resp.Header.Set("Retry-After", retryAfter)
		}
		return modelerrors.WrapHTTPError(status, resp, enriched)
	}
	return fmt.Errorf("failed to connect to MCP server: %w", err)
}

// SetManagedOAuth sets whether OAuth should be handled in managed mode.
// In managed mode, the client handles the OAuth flow instead of the server.
func (c *remoteMCPClient) SetManagedOAuth(managed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.managed = managed
}

// SetStandaloneSSE opts this client in (or out) of the streamable
// transport's standalone SSE GET stream. Takes effect on the next
// Initialize (i.e. the next connect/reconnect).
func (c *remoteMCPClient) SetStandaloneSSE(enable bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.standaloneSSE = enable
}

// SetUnmanagedOAuthRedirectURI sets the redirect URI docker-agent advertises
// when running the OAuth flow in unmanaged mode. See OAuthCapable for full
// semantics.
func (c *remoteMCPClient) SetUnmanagedOAuthRedirectURI(uri string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unmanagedOAuthRedirectURI = uri
}

// createHTTPClient creates an HTTP client with custom headers and OAuth support.
// Header values may contain ${env.NAME} and ${headers.NAME} placeholders that
// are resolved on every request (see expandHeaders), so dynamically-provided
// values never go stale on a long-lived connection.
//
// The oauthTransport is returned alongside the client so callers can inspect
// the most recent server-side failure (via lastServerErrorSnapshot) when
// Connect() returns a bare HTTP-status error and we need to surface the
// actual cause.
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
		requestElicitation:        c.requestElicitation,
		onOAuthSuccess:            c.oauthSuccess,
		tokenStore:                c.tokenStore,
		baseURL:                   c.url,
		managed:                   c.managed,
		unmanagedOAuthRedirectURI: c.unmanagedOAuthRedirectURI,
		oauthConfig:               c.oauthConfig,
		oauthHTTPClient:           oauthHTTPClientWithHeaders(c.url, c.headers, c.allowPrivateIPs, c.expandHeaders),
	}

	// Persist cookies across requests
	// So sticky sessions work if implemented by the server (e.g. in a multiple replica setup)
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("creating cookie jar: %w", err)
	}
	return &http.Client{Transport: httpclient.WrapWithOTel(oauthT), Jar: jar}, oauthT, nil
}

// expandHeaders resolves the configured header values for one outbound
// request: ${env.X} placeholders first (via the toolset's environment
// provider, when one was supplied), then ${headers.X} placeholders (via
// upstream headers carried in ctx). Running once per request keeps
// env-backed values (e.g. short-lived tokens from a dynamic provider)
// fresh on a long-lived connection.
//
// Env expansion deliberately runs first, on the configured values only,
// so untrusted upstream header values are never fed through the JS
// expander. Placeholders that cannot be resolved are left as-is.
func (c *remoteMCPClient) expandHeaders(ctx context.Context, headers map[string]string) map[string]string {
	if c.envExpander != nil {
		headers = c.envExpander.ExpandMap(ctx, headers)
	}
	return upstream.ResolveHeaders(ctx, headers)
}

func (c *remoteMCPClient) headerTransport() http.RoundTripper {
	base := httpclient.TransportForAllowPrivateIPs(c.allowPrivateIPs)
	origin := c.url
	if path, ok := unixSocketPath(c.url); ok {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		base = t
		origin = "http://localhost/"
	}
	if len(c.headers) > 0 {
		return upstream.NewHeaderTransportWithResolverForOrigin(base, origin, c.headers, c.expandHeaders)
	}
	return base
}
