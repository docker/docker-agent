package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/upstream"
)

type remoteMCPClient struct {
	sessionClient
	url           string
	transportType string
	headers       map[string]string
	tokenStore    OAuthTokenStore
	managed       bool
	oauthConfig   *RemoteOAuthConfig
}

func newRemoteClient(url, transportType string, headers map[string]string, tokenStore OAuthTokenStore) *remoteMCPClient {
	slog.Debug("Creating remote MCP client", "url", url, "transport", transportType, "headers", headers)

	if tokenStore == nil {
		tokenStore = NewInMemoryTokenStore()
	}

	return &remoteMCPClient{
		url:           url,
		transportType: transportType,
		headers:       headers,
		tokenStore:    tokenStore,
	}
}

// WithOAuthConfig sets the explicit OAuth configuration for this client
func (c *remoteMCPClient) WithOAuthConfig(cfg *RemoteOAuthConfig) *remoteMCPClient {
	c.oauthConfig = cfg
	return c
}

func (c *remoteMCPClient) Initialize(ctx context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	// Create HTTP client with OAuth support
	httpClient := c.createHTTPClient()

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
		return nil, fmt.Errorf("failed to connect to MCP server: %w", err)
	}

	c.setSession(session)

	slog.Debug("Remote MCP client connected successfully")
	return session.InitializeResult(), nil
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
func (c *remoteMCPClient) createHTTPClient() *http.Client {
	transport := c.headerTransport()

	// Then wrap with OAuth support
	transport = &oauthTransport{
		base:        transport,
		client:      c,
		tokenStore:  c.tokenStore,
		baseURL:     c.url,
		managed:     c.managed,
		oauthConfig: c.oauthConfig,
	}

	return &http.Client{
		Transport: transport,
	}
}

func (c *remoteMCPClient) headerTransport() http.RoundTripper {
	if len(c.headers) > 0 {
		return upstream.NewHeaderTransport(http.DefaultTransport, c.headers)
	}
	return http.DefaultTransport
}
