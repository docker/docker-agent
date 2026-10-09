package mcp

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
	"github.com/docker/docker-agent/pkg/upstream"
)

// NewSessionRemoteToolset creates an owned, public-network-only connection using
// literal headers. Authentication and replacement belong to the caller, not OAuth.
func NewSessionRemoteToolset(name, endpoint, transport string, headers map[string]string) *Toolset {
	client := &sessionRemoteClient{
		sessionClient: sessionClient{serverAddress: sanitizeRemoteAddress(endpoint), sanitizeError: sessionRemoteError},
		endpoint:      endpoint, transport: transport, headers: maps.Clone(headers), base: httpclient.TransportForAllowPrivateIPs(false),
	}
	ts := &Toolset{name: name, mcpClient: client, logID: sanitizeRemoteAddress(endpoint), description: buildRemoteDescription(endpoint, transport), sessionOwned: true}
	ts.supervisor = newSupervisor(ts, lifecycle.Policy{Restart: lifecycle.RestartNever})
	return ts
}

type sessionRemoteClient struct {
	sessionClient

	endpoint   string
	transport  string
	headers    map[string]string
	base       http.RoundTripper
	mu         sync.Mutex
	cancel     context.CancelFunc
	connection gomcp.Connection
	http       *sessionRemoteHTTP
}

func (c *sessionRemoteClient) Initialize(ctx context.Context, _ *gomcp.InitializeRequest) (*gomcp.InitializeResult, error) {
	parent := cancellableParentFromContext(ctx)
	if parent == nil {
		parent = ctx
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &sessionRemoteHTTP{ctx: lifetime, endpoint: c.endpoint, headers: c.headers, base: c.base}
	c.mu.Lock()
	c.cancel, c.http = cancel, h
	c.mu.Unlock()
	stopped := make(chan struct{})
	stop := context.AfterFunc(parent, func() { c.forceClose(); close(stopped) })
	defer func() {
		if stop != nil && !stop() {
			<-stopped
		}
	}()
	jar, _ := cookiejar.New(nil)
	// HTTP tracing records full URLs; MCP spans retain only the sanitized host.
	httpClient := &http.Client{Transport: h, Jar: jar, CheckRedirect: httpclient.BoundedRedirects(10)}
	var transport gomcp.Transport
	switch c.transport {
	case "sse":
		transport = &gomcp.SSEClientTransport{Endpoint: c.endpoint, HTTPClient: httpClient}
	case "streamable":
		transport = &gomcp.StreamableClientTransport{Endpoint: c.endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1}
	default:
		cancel()
		return nil, errors.New("unsupported session MCP transport")
	}
	changed, prompts := c.notificationHandlers()
	opts := &gomcp.ClientOptions{ElicitationHandler: c.handleElicitationRequest, ToolListChangedHandler: changed, PromptListChangedHandler: prompts}
	c.applySamplingHandlerOpts(opts)
	client := gomcp.NewClient(&gomcp.Implementation{Name: "docker agent", Version: "1.0.0"}, opts)
	session, err := client.Connect(lifetime, &sessionRemoteTransport{Transport: transport, owner: c}, nil)
	if err != nil {
		c.forceClose()
		h.wait()
		return nil, sessionRemoteError(err)
	}
	disarmed := stop()
	if disarmed {
		stop = nil
	}
	if !disarmed || parent.Err() != nil {
		c.forceClose()
		_ = session.Close()
		h.wait()
		return nil, parent.Err()
	}
	c.setSession(session)
	return session.InitializeResult(), nil
}

// Preserve the SDK's concrete connection and its private initialization hooks.
type sessionRemoteTransport struct {
	gomcp.Transport

	owner *sessionRemoteClient
}

func (t *sessionRemoteTransport) Connect(ctx context.Context) (gomcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if conn != nil {
		t.owner.mu.Lock()
		t.owner.connection = conn
		canceled := ctx.Err() != nil
		t.owner.mu.Unlock()
		if canceled {
			_ = conn.Close()
		}
	}
	return conn, err
}

func (c *sessionRemoteClient) forceClose() {
	c.mu.Lock()
	cancel, conn := c.cancel, c.connection
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *sessionRemoteClient) Wait() error {
	return sessionRemoteError(c.sessionClient.Wait())
}

func (c *sessionRemoteClient) Close(ctx context.Context) error {
	err := c.sessionClient.Close(ctx)
	c.forceClose()
	c.mu.Lock()
	h := c.http
	c.mu.Unlock()
	if h != nil {
		h.wait()
	}
	return sessionRemoteError(err)
}

// SDK errors may embed request URLs (including query credentials) as plain text.
func sessionRemoteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("client remote MCP request failed")
}

type sessionRemoteHTTP struct {
	ctx      context.Context //nolint:containedctx // owned by one connection, canceled and drained on close
	endpoint string
	headers  map[string]string
	base     http.RoundTripper
	mu       sync.Mutex
	closed   bool
	requests sync.WaitGroup
}

func (h *sessionRemoteHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if !upstream.SameOrigin(h.endpoint, req.URL) || req.URL.User != nil || req.URL.Fragment != "" {
		return nil, errors.New("client MCP request must remain on its configured origin")
	}
	h.mu.Lock()
	if h.closed || h.ctx.Err() != nil {
		h.mu.Unlock()
		return nil, context.Canceled
	}
	h.requests.Add(1)
	h.mu.Unlock()
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(h.ctx, cancel)
	finish := sync.OnceFunc(func() { stop(); cancel(); h.requests.Done() })
	clone := req.Clone(ctx)
	for name, value := range h.headers {
		clone.Header.Set(name, value)
	}
	resp, err := h.base.RoundTrip(clone)
	if err != nil {
		finish()
		return nil, sessionRemoteError(err)
	}
	resp.Body = &sessionRemoteBody{ReadCloser: resp.Body, finish: finish}
	return resp, nil
}

func (h *sessionRemoteHTTP) wait() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.requests.Wait()
}

type sessionRemoteBody struct {
	io.ReadCloser

	finish func()
}

func (b *sessionRemoteBody) Close() error {
	defer b.finish()
	return b.ReadCloser.Close()
}
