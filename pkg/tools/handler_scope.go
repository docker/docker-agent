package tools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HandlerScope binds host callbacks to one runtime request. Toolsets keep
// stable dispatchers and resolve the actual owner from the operation context.
type HandlerScope struct {
	Elicitation  ElicitationHandler
	OAuthSuccess func()
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

func NotifyScopedOAuthSuccess(ctx context.Context) {
	if scope, ok := HandlerScopeFrom(ctx); ok && scope.OAuthSuccess != nil {
		scope.OAuthSuccess()
	}
}
