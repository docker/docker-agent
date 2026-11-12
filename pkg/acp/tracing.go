package acp

import (
	"context"
	"maps"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/version"
)

// Trace metadata is request-scoped. Never import client baggage or arbitrary meta.
func startACPRequest(ctx context.Context, method string, meta map[string]any) (context.Context, trace.Span) {
	carrier := propagation.MapCarrier{}
	for _, key := range []string{"traceparent", "tracestate"} {
		if value, ok := meta[key].(string); ok && len(value) <= 512 {
			carrier[key] = value
		}
	}
	ctx = (propagation.TraceContext{}).Extract(ctx, carrier)
	ctx, span := otel.Tracer(version.AppName).Start(ctx, "acp."+method,
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("rpc.method", method)))
	// Downstream code may annotate its current span with paths or raw errors.
	// Carry parentage without giving it the writable ACP boundary span.
	return trace.ContextWithSpanContext(ctx, span.SpanContext()), span
}

func traceMeta(ctx context.Context, meta map[string]any) map[string]any {
	result := maps.Clone(meta)
	if result == nil {
		result = make(map[string]any)
	}
	// Metadata may have come from an MCP peer or an earlier attempt.
	delete(result, "traceparent")
	delete(result, "tracestate")
	delete(result, "baggage")
	carrier := propagation.MapCarrier{}
	(propagation.TraceContext{}).Inject(ctx, carrier)
	for key, value := range carrier {
		result[key] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
