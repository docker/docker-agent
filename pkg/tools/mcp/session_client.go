package mcp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/tools"
)

// sessionClient provides shared session-management logic for MCP client
// implementations. Both stdioMCPClient and remoteMCPClient embed it to avoid
// duplicating the session-nil guards, notification handlers, and delegating
// methods.
type sessionClient struct {
	session                  *gomcp.ClientSession
	toolListChangedHandler   func()
	promptListChangedHandler func()
	elicitationHandler       tools.ElicitationHandler
	oauthSuccessHandler      func()
	mu                       sync.RWMutex
}

// setSession stores the session under the write lock.
func (c *sessionClient) setSession(s *gomcp.ClientSession) {
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
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
	c.toolListChangedHandler = handler
	c.mu.Unlock()
}

func (c *sessionClient) SetPromptListChangedHandler(handler func()) {
	c.mu.Lock()
	c.promptListChangedHandler = handler
	c.mu.Unlock()
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
	if s := c.getSession(); s != nil {
		return s.Tools(ctx, request)
	}
	return func(yield func(*gomcp.Tool, error) bool) {
		yield(nil, errors.New("session not initialized"))
	}
}

func (c *sessionClient) CallTool(ctx context.Context, request *gomcp.CallToolParams) (*gomcp.CallToolResult, error) {
	if s := c.getSession(); s != nil {
		return s.CallTool(ctx, request)
	}
	return nil, errors.New("session not initialized")
}

func (c *sessionClient) ListPrompts(ctx context.Context, request *gomcp.ListPromptsParams) iter.Seq2[*gomcp.Prompt, error] {
	if s := c.getSession(); s != nil {
		return s.Prompts(ctx, request)
	}
	return func(yield func(*gomcp.Prompt, error) bool) {
		yield(nil, errors.New("session not initialized"))
	}
}

func (c *sessionClient) GetPrompt(ctx context.Context, request *gomcp.GetPromptParams) (*gomcp.GetPromptResult, error) {
	if s := c.getSession(); s != nil {
		return s.GetPrompt(ctx, request)
	}
	return nil, errors.New("session not initialized")
}

// handleElicitationRequest forwards incoming elicitation requests from the MCP
// server to the registered handler. It is used as the gomcp ElicitationHandler
// callback for both stdio and remote clients.
func (c *sessionClient) handleElicitationRequest(ctx context.Context, req *gomcp.ElicitRequest) (*gomcp.ElicitResult, error) {
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
	c.elicitationHandler = handler
	c.mu.Unlock()
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
	c.oauthSuccessHandler = handler
	c.mu.Unlock()
}

// oauthSuccess invokes the registered OAuth success handler, if any.
func (c *sessionClient) oauthSuccess() {
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
