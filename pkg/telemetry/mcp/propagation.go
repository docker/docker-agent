package mcp

import (
	"context"
	"maps"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// metaCarrier adapts an MCP `params._meta` map (which the MCP SDK exposes
// as `map[string]any`) to OTel's TextMapCarrier interface so the package's
// configured propagator can read and write trace context (`traceparent`,
// `tracestate`, `baggage`) the way it does for any HTTP carrier.
type metaCarrier struct {
	meta map[string]any
}

func (c metaCarrier) Get(key string) string {
	if c.meta == nil {
		return ""
	}
	v, ok := c.meta[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (c metaCarrier) Set(key, value string) {
	if c.meta == nil {
		return
	}
	c.meta[key] = value
}

func (c metaCarrier) Keys() []string {
	if c.meta == nil {
		return nil
	}
	keys := make([]string, 0, len(c.meta))
	for k, v := range c.meta {
		if _, ok := v.(string); ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// InjectMeta writes the active trace context into the given MCP `_meta`
// map so the receiving server can extract it and parent its SERVER span
// onto our CLIENT span. Per the MCP semconv, the keys written are
// `traceparent`, `tracestate`, and `baggage` (W3C TraceContext + Baggage).
//
// If meta is nil, InjectMeta is a no-op — callers should ensure the map
// is non-nil before calling so the keys actually persist on the request.
func InjectMeta(ctx context.Context, meta map[string]any) {
	if meta == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, metaCarrier{meta: meta})
}

// ExtractMeta reads trace context from the given MCP `_meta` map and
// returns a context with the parent span attached. Use on the server side
// to chain incoming spans onto the client's caller.
func ExtractMeta(ctx context.Context, meta map[string]any) context.Context {
	if meta == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, metaCarrier{meta: meta})
}

// EnsureMeta returns a metadata map suitable for InjectMeta to write
// trace context into. When m is non-nil it is shallow-copied so an
// upstream caller that reuses the same request struct (e.g. on retry)
// does not see stale `traceparent` keys from a previous span injected
// into the map they own. When m is nil a fresh map is allocated.
func EnsureMeta(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m)+3)
	maps.Copy(out, m)
	return out
}

// Verify metaCarrier satisfies the propagator interface at compile time.
var _ propagation.TextMapCarrier = metaCarrier{}
