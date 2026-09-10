package tools

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

// InteractivePromptsAllowed reports whether the context allows blocking on
// user-driven flows. The default is true so existing callers (RunStream,
// tests) keep working without changes.
//
// It is the read-side companion to [WithoutInteractivePrompts]: the runtime
// sets the marker on non-interactive sessions (background agents, MCP serve,
// A2A) so the OAuth transport can fail fast instead of raising an elicitation
// that no one can answer.
func InteractivePromptsAllowed(ctx context.Context) bool {
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
//
// PartialStartError and TotalStartError batch several inner-toolset
// failures and only count as authorization-required when all of their
// causes are (see the AuthOnly fields): matching a mixed batch through
// plain errors.As would hide the non-auth causes behind the silent
// auth-deferral handling.
func IsAuthorizationRequired(err error) bool {
	var partial *PartialStartError
	if errors.As(err, &partial) {
		return partial.AuthOnly
	}
	var total *TotalStartError
	if errors.As(err, &total) {
		return total.AuthOnly
	}
	var target *AuthorizationRequiredError
	return errors.As(err, &target)
}

// OAuthDeclinedError is returned by the transport when the user explicitly
// declines or cancels an interactive OAuth authorization flow (e.g. by
// clicking "Cancel" on the host's Authentication Request dialog).
//
// It is intentionally distinct from AuthorizationRequiredError: the latter
// is a silent deferral pending an interactive context, while a decline is
// a deliberate user action. Callers MUST NOT immediately retry the OAuth
// flow on a decline — that would re-emit the dialog the user just
// dismissed, which is exactly the bug this sentinel exists to prevent.
//
// Typical handling is for the caller (e.g. the MCP catalog toolset) to
// remove the server from its active set so subsequent tool enumerations
// don't kick off a fresh OAuth flow. Re-enabling the server (e.g. via
// the catalog's enable meta-tool) is the natural way for the user to
// say "actually, please retry".
type OAuthDeclinedError struct {
	URL string
}

func (e *OAuthDeclinedError) Error() string {
	if e.URL == "" {
		return "OAuth authorization was declined or cancelled by the user"
	}
	return "OAuth authorization to " + e.URL + " was declined or cancelled by the user"
}

// IsOAuthDeclined reports whether err (or any error wrapped by it) signals
// that the user explicitly declined or cancelled an interactive OAuth
// authorization flow. Callers use this to break the
// "Tools() -> Start() -> OAuth elicitation" retry loop so the dismissed
// dialog does not immediately re-appear on the next loop iteration.
func IsOAuthDeclined(err error) bool {
	var target *OAuthDeclinedError
	return errors.As(err, &target)
}
