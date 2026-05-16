package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/upstream"
)

type remoteMCPClient struct {
	sessionClient

	url           string
	transportType string
	headers       map[string]string
	tokenStore    OAuthTokenStore
	managed       bool
	oauthConfig   *latest.RemoteOAuthConfig
}

func newRemoteClient(url, transportType string, headers map[string]string, tokenStore OAuthTokenStore, oauthConfig *latest.RemoteOAuthConfig) *remoteMCPClient {
	slog.Debug("Creating remote MCP client", "url", url, "transport", transportType, "headers", headers)

	if tokenStore == nil {
		tokenStore = NewInMemoryTokenStore()
	}

	return &remoteMCPClient{
		url:           url,
		transportType: transportType,
		headers:       headers,
		tokenStore:    tokenStore,
		oauthConfig:   oauthConfig,
	}
}

func (c *remoteMCPClient) Initialize(ctx context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	// Create HTTP client with OAuth support. We keep a reference to the
	// oauthTransport so we can enrich Connect errors with the server's own
	// explanation — without this, a plain `Bad Request` bubbles up and the
	// user has no idea that, say, the Slack app hasn't been enabled for MCP.
	httpClient, oauthT := c.createHTTPClient()

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

// createHTTPClient creates an HTTP client with custom headers and OAuth support.
// Header values may contain ${headers.NAME} placeholders that are resolved
// at request time from upstream headers stored in the request context.
//
// The oauthTransport is returned alongside the client so callers can inspect
// the most recent server-side failure (via lastServerError) when Connect()
// returns a bare HTTP-status error and we need to surface the actual cause.
func (c *remoteMCPClient) createHTTPClient() (*http.Client, *oauthTransport) {
	base := c.headerTransport()

	// Then wrap with OAuth support
	oauthT := &oauthTransport{
		base:        base,
		client:      c,
		tokenStore:  c.tokenStore,
		baseURL:     c.url,
		managed:     c.managed,
		oauthConfig: c.oauthConfig,
	}

	return &http.Client{Transport: oauthT}, oauthT
}

func (c *remoteMCPClient) headerTransport() http.RoundTripper {
	if len(c.headers) > 0 {
		return upstream.NewHeaderTransport(http.DefaultTransport, c.headers)
	}
	return http.DefaultTransport
}
