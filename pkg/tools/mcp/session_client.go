package mcp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"

	otelmcp "github.com/docker/docker-agent/pkg/telemetry/mcp"
	"github.com/docker/docker-agent/pkg/tools"
)

// sessionClient provides shared session-management logic for MCP client
// implementations. Both stdioMCPClient and remoteMCPClient embed it to avoid
// duplicating the session-nil guards, notification handlers, and delegating
// methods.
//
// `serverAddress` is captured at construction time (the remote URL for
// HTTP/SSE clients, the executable name for stdio clients) and stamped on
// every CLIENT-kind MCP span as the OTel `server.address` attribute. Without
// it, a `tools/list` failure span carries `mcp.method.name=tools/list` and
// nothing else identifying which target produced the error — useful in a
// single-MCP agent, useless in any agent wired to two or more.
type sessionClient struct {
	session                  *gomcp.ClientSession
	serverAddress            string
	toolListChangedHandler   func()
	promptListChangedHandler func()
	elicitationHandler       tools.ElicitationHandler
	samplingHandler          tools.SamplingHandler
	samplingWithToolsHandler tools.SamplingWithToolsHandler
	oauthSuccessHandler      func()
	inflight                 map[uint64]tools.HandlerScope
	nextInflight             uint64
	mu                       sync.RWMutex
}

// setSession stores the session under the write lock.
func (c *sessionClient) setSession(s *gomcp.ClientSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = s
}

// ServerAddress returns the connection identifier captured at construction
// time (URL for remote clients, executable name for stdio). Exposed so
// the parent `toolset.start` span can stamp it as `server.address` —
// otherwise an Initialize failure surfaces the error message but no
// indication of which MCP target produced it.
func (c *sessionClient) ServerAddress() string {
	return c.serverAddress
}

// getSession returns the current session under the read lock.
func (c *sessionClient) getSession() *gomcp.ClientSession {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

// notificationHandlers returns ToolListChanged and PromptListChanged closures
// suitable for gomcp.ClientOptions. They read the registered handler under the
// read lock and invoke it if non-nil.
func (c *sessionClient) notificationHandlers() (
	toolChanged func(context.Context, *gomcp.ToolListChangedRequest),
	promptChanged func(context.Context, *gomcp.PromptListChangedRequest),
) {
	toolChanged = func(_ context.Context, _ *gomcp.ToolListChangedRequest) {
		c.mu.RLock()
		h := c.toolListChangedHandler
		c.mu.RUnlock()
		if h != nil {
			h()
		}
	}
	promptChanged = func(_ context.Context, _ *gomcp.PromptListChangedRequest) {
		c.mu.RLock()
		h := c.promptListChangedHandler
		c.mu.RUnlock()
		if h != nil {
			h()
		}
	}
	return toolChanged, promptChanged
}

func (c *sessionClient) SetToolListChangedHandler(handler func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolListChangedHandler = handler
}

func (c *sessionClient) SetPromptListChangedHandler(handler func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.promptListChangedHandler = handler
}

func (c *sessionClient) Wait() error {
	if s := c.getSession(); s != nil {
		return s.Wait()
	}
	return nil
}

func (c *sessionClient) Close(context.Context) error {
	if s := c.getSession(); s != nil {
		return s.Close()
	}
	return nil
}

func (c *sessionClient) ListTools(ctx context.Context, request *gomcp.ListToolsParams) iter.Seq2[*gomcp.Tool, error] {
	s := c.getSession()
	if s == nil {
		return func(yield func(*gomcp.Tool, error) bool) {
			yield(nil, errors.New("session not initialized"))
		}
	}
	// Start the span and the underlying RPC inside the closure so a
	// caller that obtains the iterator and never iterates does not
	// leak the span (and the in-flight RPC). Span lifetime now equals
	// iteration lifetime.
	return func(yield func(*gomcp.Tool, error) bool) {
		spanCtx, span := otelmcp.StartClient(ctx, otelmcp.CallOptions{
			Method:        otelmcp.MethodToolsList,
			SessionID:     s.ID(),
			ServerAddress: c.serverAddress,
		})
		defer span.End()

		// Stamp the tool count on the span when iteration finishes —
		// answers "what did this server actually return?" without
		// having to walk into the JSON-RPC payload. Counts only the
		// tools the iterator yielded successfully; partial counts are
		// preserved when the caller breaks out early.
		var count int
		defer func() {
			span.SetAttributes(attribute.Int("cagent.mcp.tools.count", count))
		}()

		if request != nil {
			request.Meta = otelmcp.EnsureMeta(request.Meta)
			otelmcp.InjectMeta(spanCtx, request.Meta)
		}
		for tool, err := range s.Tools(spanCtx, request) {
			if err != nil {
				// Record each error inline rather than only the
				// last one — paginated lists may yield multiple
				// failures and the trace should reflect them all.
				span.RecordError(err, "")
			} else if tool != nil {
				count++
			}
			if !yield(tool, err) {
				return
			}
		}
	}
}

func (c *sessionClient) CallTool(ctx context.Context, request *gomcp.CallToolParams) (*gomcp.CallToolResult, error) {
	s := c.getSession()
	if s == nil {
		return nil, errors.New("session not initialized")
	}
	opts := otelmcp.CallOptions{
		Method:        otelmcp.MethodToolsCall,
		SessionID:     s.ID(),
		ServerAddress: c.serverAddress,
	}
	if request != nil {
		opts.ToolName = request.Name
	}
	spanCtx, span := otelmcp.StartClient(ctx, opts)
	defer span.End()

	if request != nil {
		request.Meta = otelmcp.EnsureMeta(request.Meta)
		otelmcp.InjectMeta(spanCtx, request.Meta)
	}

	callID := c.registerCallContext(spanCtx)
	defer c.unregisterCallContext(callID)
	result, err := s.CallTool(spanCtx, request)
	if err != nil {
		span.RecordError(err, "")
	}
	return result, err
}

func (c *sessionClient) ListPrompts(ctx context.Context, request *gomcp.ListPromptsParams) iter.Seq2[*gomcp.Prompt, error] {
	s := c.getSession()
	if s == nil {
		return func(yield func(*gomcp.Prompt, error) bool) {
			yield(nil, errors.New("session not initialized"))
		}
	}
	return func(yield func(*gomcp.Prompt, error) bool) {
		// Span and RPC start at iteration time so an unused
		// iterator never leaks either.
		spanCtx, span := otelmcp.StartClient(ctx, otelmcp.CallOptions{
			Method:        otelmcp.MethodPromptsList,
			SessionID:     s.ID(),
			ServerAddress: c.serverAddress,
		})
		defer span.End()

		if request != nil {
			request.Meta = otelmcp.EnsureMeta(request.Meta)
			otelmcp.InjectMeta(spanCtx, request.Meta)
		}
		for prompt, err := range s.Prompts(spanCtx, request) {
			if err != nil {
				span.RecordError(err, "")
			}
			if !yield(prompt, err) {
				return
			}
		}
	}
}

func (c *sessionClient) GetPrompt(ctx context.Context, request *gomcp.GetPromptParams) (*gomcp.GetPromptResult, error) {
	s := c.getSession()
	if s == nil {
		return nil, errors.New("session not initialized")
	}
	opts := otelmcp.CallOptions{
		Method:        otelmcp.MethodPromptsGet,
		SessionID:     s.ID(),
		ServerAddress: c.serverAddress,
	}
	if request != nil {
		opts.PromptName = request.Name
	}
	spanCtx, span := otelmcp.StartClient(ctx, opts)
	defer span.End()

	if request != nil {
		request.Meta = otelmcp.EnsureMeta(request.Meta)
		otelmcp.InjectMeta(spanCtx, request.Meta)
	}

	result, err := s.GetPrompt(spanCtx, request)
	if err != nil {
		span.RecordError(err, "")
	}
	return result, err
}

func (c *sessionClient) registerCallContext(ctx context.Context) uint64 {
	scope, scoped := tools.HandlerScopeFrom(ctx)
	if !scoped {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextInflight++
	if c.inflight == nil {
		c.inflight = make(map[uint64]tools.HandlerScope)
	}
	c.inflight[c.nextInflight] = scope
	return c.nextInflight
}

func (c *sessionClient) unregisterCallContext(id uint64) {
	if id == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
}

// requestContext returns the sole active tool call's context. MCP does not
// identify which call caused a server-initiated request, so concurrent calls
// deliberately fall back to an unscoped context rather than misroute it.
func (c *sessionClient) requestContext(fallback context.Context) context.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.inflight) != 1 {
		return tools.WithoutHandlerScope(fallback)
	}
	for _, scope := range c.inflight {
		return tools.WithHandlerScope(fallback, scope)
	}
	return tools.WithoutHandlerScope(fallback)
}

// handleElicitationRequest forwards incoming elicitation requests from the MCP
// server to the registered handler. It is used as the gomcp ElicitationHandler
// callback for both stdio and remote clients.
func (c *sessionClient) handleElicitationRequest(ctx context.Context, req *gomcp.ElicitRequest) (*gomcp.ElicitResult, error) {
	ctx = c.requestContext(ctx)
	slog.DebugContext(ctx, "Received elicitation request from MCP server", "message", req.Params.Message)

	c.mu.RLock()
	handler := c.elicitationHandler
	c.mu.RUnlock()

	if handler == nil {
		return nil, errors.New("no elicitation handler configured")
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		return nil, fmt.Errorf("elicitation failed: %w", err)
	}

	return &gomcp.ElicitResult{
		Action:  string(result.Action),
		Content: result.Content,
	}, nil
}

// SetElicitationHandler sets the handler that processes elicitation requests
// from the MCP server.
func (c *sessionClient) SetElicitationHandler(handler tools.ElicitationHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elicitationHandler = handler
}

// handleSamplingRequest forwards incoming sampling/createMessage requests
// from the MCP server to the registered handler. It is used as the gomcp
// CreateMessageHandler callback for both stdio and remote clients.
func (c *sessionClient) handleSamplingRequest(ctx context.Context, req *gomcp.CreateMessageRequest) (*gomcp.CreateMessageResult, error) {
	ctx = c.requestContext(ctx)
	slog.DebugContext(ctx, "Received sampling request from MCP server", "messages", len(req.Params.Messages))

	c.mu.RLock()
	handler := c.samplingHandler
	c.mu.RUnlock()

	if handler == nil {
		return nil, errors.New("no sampling handler configured")
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		return nil, fmt.Errorf("sampling failed: %w", err)
	}

	return result, nil
}

// SetSamplingHandler sets the handler that processes sampling requests
// from the MCP server.
func (c *sessionClient) SetSamplingHandler(handler tools.SamplingHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samplingHandler = handler
}

// handleSamplingWithToolsRequest forwards incoming sampling/createMessage
// requests that may include tools to the registered handler. It is used as
// the gomcp CreateMessageWithToolsHandler callback for both stdio and remote
// clients when the with-tools handler is registered.
func (c *sessionClient) handleSamplingWithToolsRequest(ctx context.Context, req *gomcp.CreateMessageWithToolsRequest) (*gomcp.CreateMessageWithToolsResult, error) {
	ctx = c.requestContext(ctx)
	slog.DebugContext(ctx, "Received sampling-with-tools request from MCP server",
		"messages", len(req.Params.Messages),
		"tools", len(req.Params.Tools),
	)

	c.mu.RLock()
	handler := c.samplingWithToolsHandler
	c.mu.RUnlock()

	if handler == nil {
		return nil, errors.New("no sampling-with-tools handler configured")
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		return nil, fmt.Errorf("sampling failed: %w", err)
	}

	return result, nil
}

// SetSamplingWithToolsHandler sets the handler that processes sampling
// requests carrying a tools array from the MCP server.
func (c *sessionClient) SetSamplingWithToolsHandler(handler tools.SamplingWithToolsHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samplingWithToolsHandler = handler
}

// applySamplingHandlerOpts wires the SDK CreateMessage* callback into opts.
//
// The SDK panics if both CreateMessageHandler and CreateMessageWithToolsHandler
// are populated, so we register exactly one. The with-tools callback is the
// default — it gets registered even when both handler fields are still nil at
// Initialize time. The callback itself reads c.samplingWithToolsHandler /
// c.samplingHandler under c.mu at request time (see handleSamplingWithToolsRequest
// above), so a SetSampling*Handler call that lands after Initialize — for
// example, on a supervisor-driven reconnect that races configureToolsetHandlers
// — still takes effect without a re-init. We only fall back to the basic
// CreateMessageHandler when the caller has demonstrated that only the basic
// path will ever be used (i.e. samplingHandler is set and samplingWithToolsHandler
// is not).
func (c *sessionClient) applySamplingHandlerOpts(opts *gomcp.ClientOptions) {
	c.mu.RLock()
	basicOnly := c.samplingHandler != nil && c.samplingWithToolsHandler == nil
	c.mu.RUnlock()
	if basicOnly {
		opts.CreateMessageHandler = c.handleSamplingRequest
		return
	}
	opts.CreateMessageWithToolsHandler = c.handleSamplingWithToolsRequest
}

// requestElicitation invokes the registered elicitation handler directly.
// This is used by the OAuth transport to trigger elicitation outside of
// the normal MCP request flow.
//
// When no handler is wired up (typically because the OAuth flow ran before
// the runtime had a chance to attach its elicitation bridge — e.g. during
// a startup probe whose context lost the WithoutInteractivePrompts marker),
// we surface the recognisable AuthorizationRequiredError sentinel rather
// than a bare "no elicitation handler configured" error. That keeps the
// failure mode of "client side not ready yet" identical to the explicit
// non-interactive deferral: the toolset is flagged as needing auth and
// silently retried on the next conversation turn, instead of bubbling a
// confusing message up to the user.
func (c *sessionClient) requestElicitation(ctx context.Context, req *gomcp.ElicitParams) (tools.ElicitationResult, error) {
	c.mu.RLock()
	handler := c.elicitationHandler
	c.mu.RUnlock()

	if handler == nil {
		slog.DebugContext(ctx, "OAuth flow requested elicitation before the runtime wired up a handler; deferring")
		return tools.ElicitationResult{}, &AuthorizationRequiredError{}
	}

	return handler(ctx, req)
}

// SetOAuthSuccessHandler sets the handler called when an OAuth flow completes.
func (c *sessionClient) SetOAuthSuccessHandler(handler func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.oauthSuccessHandler = handler
}

// oauthSuccess invokes the request-scoped OAuth success callback, then falls
// back to the registered default for contexts without a scope.
func (c *sessionClient) oauthSuccess(ctx context.Context) {
	if tools.HasHandlerScope(ctx) {
		tools.NotifyScopedOAuthSuccess(ctx)
		return
	}
	c.mu.RLock()
	handler := c.oauthSuccessHandler
	c.mu.RUnlock()

	if handler != nil {
		handler()
	}
}

// SetManagedOAuth is a no-op at the session level. The remoteMCPClient
// overrides this to store the managed flag for its OAuth transport.
func (c *sessionClient) SetManagedOAuth(bool) {}

// SetUnmanagedOAuthRedirectURI is a no-op at the session level. The
// remoteMCPClient overrides this to store the URI for its OAuth transport.
// Stdio MCP clients never run OAuth (they have no HTTP transport to
// authenticate), so the URI is ignored there too.
func (c *sessionClient) SetUnmanagedOAuthRedirectURI(string) {}
