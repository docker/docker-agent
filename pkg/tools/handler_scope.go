package tools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HandlerScope binds host callbacks to one runtime request. Toolsets keep
// stable dispatchers and resolve the actual owner from the operation context.
type HandlerScope struct {
	Elicitation       ElicitationHandler
	Sampling          SamplingHandler
	SamplingWithTools SamplingWithToolsHandler
	OAuthSuccess      func()
}

type (
	handlerScopeKey        struct{}
	withoutHandlerScopeKey struct{}
)

func WithHandlerScope(ctx context.Context, scope HandlerScope) context.Context {
	return context.WithValue(context.WithValue(ctx, withoutHandlerScopeKey{}, false), handlerScopeKey{}, scope)
}

func WithoutHandlerScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutHandlerScopeKey{}, true)
}

func HandlerScopeFrom(ctx context.Context) (HandlerScope, bool) {
	if disabled, _ := ctx.Value(withoutHandlerScopeKey{}).(bool); disabled {
		return HandlerScope{}, false
	}
	scope, ok := ctx.Value(handlerScopeKey{}).(HandlerScope)
	return scope, ok
}

func HasHandlerScope(ctx context.Context) bool {
	_, ok := HandlerScopeFrom(ctx)
	return ok
}

func ScopedElicitationHandler(ctx context.Context, req *mcp.ElicitParams) (ElicitationResult, error) {
	if scope, ok := HandlerScopeFrom(ctx); ok && scope.Elicitation != nil {
		return scope.Elicitation(ctx, req)
	}
	return ElicitationResult{}, errors.New("no request-scoped elicitation handler configured")
}

// SamplingScopeHandler dispatches an MCP sampling request through the current
// request scope.
func SamplingScopeHandler(ctx context.Context, req *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) { //nolint:staticcheck // MCP sampling remains supported during its deprecation window.
	if scope, ok := HandlerScopeFrom(ctx); ok && scope.Sampling != nil {
		return scope.Sampling(ctx, req)
	}
	return nil, errors.New("no request-scoped sampling handler configured")
}

// SamplingWithToolsScopeHandler dispatches sampling-with-tools through the
// current request scope.
func SamplingWithToolsScopeHandler(ctx context.Context, req *mcp.CreateMessageWithToolsParams) (*mcp.CreateMessageWithToolsResult, error) { //nolint:staticcheck // MCP sampling remains supported during its deprecation window.
	if scope, ok := HandlerScopeFrom(ctx); ok && scope.SamplingWithTools != nil {
		return scope.SamplingWithTools(ctx, req)
	}
	return nil, errors.New("no request-scoped sampling-with-tools handler configured")
}

func NotifyScopedOAuthSuccess(ctx context.Context) {
	if scope, ok := HandlerScopeFrom(ctx); ok && scope.OAuthSuccess != nil {
		scope.OAuthSuccess()
	}
}
