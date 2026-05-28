package mcp

import "context"

// cancellableParentCtxKey is the private context key under which
// clientConnector.Connect stashes the caller's original ctx before
// detaching it with context.WithoutCancel.
//
// We use a struct{} key (the standard Go idiom for ctx keys) so it's
// not addressable from other packages and can't collide with anything
// else.
type cancellableParentCtxKey struct{}

// withCancellableParent attaches `parent` to `ctx` so that downstream
// code paths that explicitly opt in can observe the parent's
// cancellation signal.
//
// Why this exists
// ---------------
//
// clientConnector.Connect (see mcp.go) deliberately detaches its
// caller's ctx with context.WithoutCancel before sending the MCP
// initialize handshake. The rationale is correct: an MCP toolset's
// session must outlive any single request -- otherwise, when the
// inbound HTTP request that first caused toolset.Start to be invoked
// finishes, the underlying SSE/stdio connection would tear down, and
// subsequent agent requests would have to re-initialize from scratch.
// Worse, an OAuth flow that succeeded would still die at the moment
// its triggering request returned.
//
// But that detachment also severs a signal we actually do want to
// observe in some operations: user-initiated cancellation. In
// particular, the unmanaged OAuth flow (handleUnmanagedOAuthFlow)
// blocks on a select waiting for either the user's elicitation reply
// or an out-of-band callback. If the user aborts the in-progress
// agent run via the host's Stop affordance, the streamCtx that the
// SessionManager derived from the request ctx gets cancelled -- and
// every ctx in the chain BELOW Connect's WithoutCancel boundary
// stays alive. The OAuth select then blocks forever, the per-session
// streaming lock is never released, and the next user message
// returns 409 Conflict from RunSession.ErrSessionBusy.
//
// We resolve this without giving up the connection-longevity
// invariant by stashing the caller's original ctx as a value on the
// detached ctx. The detached ctx remains the primary driver of the
// connection's lifetime (so the WithoutCancel rationale holds), and
// any operation that wants to observe user-initiated cancellation
// can opt in by calling cancellableParentFromContext and selecting
// on its Done channel alongside the local ctx.
//
// Carrying ctx-as-value is unusual but appropriate here: the parent
// is genuinely metadata about how the detached ctx was constructed,
// not data that the request is operating on.
func withCancellableParent(ctx, parent context.Context) context.Context {
	if parent == nil {
		return ctx
	}
	return context.WithValue(ctx, cancellableParentCtxKey{}, parent)
}

// cancellableParentFromContext returns the parent ctx previously
// attached via withCancellableParent, or nil if none is set.
//
// Callers should treat a nil return as "no user-cancellation signal
// available on this code path" -- not as an error. The most common
// reason for nil is that the operation is running outside an
// MCP-toolset call site (e.g. unit tests, CLI invocations), in
// which case there is no useful user-cancellation signal to observe.
func cancellableParentFromContext(ctx context.Context) context.Context {
	parent, _ := ctx.Value(cancellableParentCtxKey{}).(context.Context)
	return parent
}
