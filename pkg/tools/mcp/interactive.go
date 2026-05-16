package mcp

import (
	"context"
	"errors"
)

// noInteractivePromptsKey is the unexported key used to attach the
// "skip interactive prompts" flag to a context.
type noInteractivePromptsKey struct{}

// WithoutInteractivePrompts returns a context that asks the MCP transport
// stack to refuse any flow that would require user input. The canonical
// example is OAuth: a remote MCP server's first contact is typically a 401
// Unauthorized that triggers an interactive elicitation flow ("approve OAuth
// authorization?"). During startup the TUI is not yet ready to surface that
// dialog, the user has no input field, and Ctrl-C cannot reach the elicitation
// goroutine because it is blocked on a synchronous send/receive.
//
// Callers that prepare data eagerly (sidebar tool counts, dry-runs, health
// checks) should wrap their context with this helper so toolset Start()
// returns a meaningful error immediately instead of hanging the process.
//
// Once a real user interaction is in progress (RunStream), the context
// should NOT carry this value so the user can complete OAuth normally.
func WithoutInteractivePrompts(ctx context.Context) context.Context {
	return context.WithValue(ctx, noInteractivePromptsKey{}, true)
}

// interactivePromptsAllowed reports whether the context allows blocking on
// user-driven flows. The default is true so existing callers (RunStream,
// tests) keep working without changes.
func interactivePromptsAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(noInteractivePromptsKey{}).(bool)
	return !v
}

// AuthorizationRequiredError is returned by the transport when an OAuth
// elicitation would be needed but the context disallows interactive prompts
// (see WithoutInteractivePrompts). Callers can detect it with
// IsAuthorizationRequired and decide how (or whether) to surface it.
//
// The exported type is also useful in tests that want to simulate the
// deferred-OAuth path without spinning up a real HTTP server.
type AuthorizationRequiredError struct {
	URL string
}

func (e *AuthorizationRequiredError) Error() string {
	return e.URL + " requires interactive OAuth authorization"
}

// IsAuthorizationRequired reports whether err (or any error wrapped by it)
// signals that the toolset failed to start because OAuth is needed and the
// caller chose to defer the prompt. Callers can use this to render a softer,
// "needs auth" notice instead of a red error.
func IsAuthorizationRequired(err error) bool {
	var target *AuthorizationRequiredError
	return errors.As(err, &target)
}
