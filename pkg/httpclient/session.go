package httpclient

import "context"

type sessionIDKey struct{}

// ContextWithSessionID returns a new context carrying the given session ID.
// When set, [userAgentTransport.RoundTrip] forwards it as the
// `X-Cagent-Session-Id` header — but only on gateway-bound requests
// (those already carrying `X-Cagent-Forward`), to keep the identifier
// out of direct provider calls and unrelated outbound HTTP.
func ContextWithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// SessionIDFromContext returns the session ID stored on ctx by
// [ContextWithSessionID], or the empty string if none is set.
func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}
