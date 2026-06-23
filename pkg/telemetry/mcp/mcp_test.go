package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace"
	traceapi "go.opentelemetry.io/otel/trace"
)

func TestEnsureMeta(t *testing.T) {
	t.Parallel()
	got := EnsureMeta(nil)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	existing := map[string]any{"foo": "bar"}
	got = EnsureMeta(existing)
	assert.Equal(t, existing, got)
}

func TestInjectExtractRoundTrip(t *testing.T) {
	// Mutates the global OTel text-map propagator, so this test cannot
	// run in parallel with other tests that read or modify it.

	// A propagator must be configured for inject/extract to do anything;
	// install one for the duration of the test and put it back after.
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	// Start a sampled span so traceparent has a non-trivial trace id.
	tp := trace.NewTracerProvider(trace.WithSampler(trace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	parentCtx, parentSpan := tp.Tracer("test").Start(t.Context(), "parent")
	defer parentSpan.End()
	parentSC := traceapi.SpanContextFromContext(parentCtx)

	meta := map[string]any{}
	InjectMeta(parentCtx, meta)
	assert.Contains(t, meta, "traceparent",
		"propagator should have written W3C traceparent into _meta")

	// Extract from a fresh context and verify the span context lines up
	// with the parent we started with.
	childCtx := ExtractMeta(t.Context(), meta)
	extracted := traceapi.SpanContextFromContext(childCtx)
	assert.Equal(t, parentSC.TraceID(), extracted.TraceID())
	assert.Equal(t, parentSC.SpanID(), extracted.SpanID())
}

func TestInjectMetaNilNoOp(t *testing.T) {
	t.Parallel()
	// Should not panic on a nil map.
	InjectMeta(t.Context(), nil)
}

func TestExtractMetaNilReturnsParent(t *testing.T) {
	t.Parallel()
	got := ExtractMeta(t.Context(), nil)
	// Without trace context to extract we get back the same context.
	assert.Equal(t, t.Context(), got)
}

func TestStartClientReturnsActiveSpan(t *testing.T) {
	// Mutates the global OTel tracer provider, so this test cannot run
	// in parallel with other tests that read or modify it.

	tp := trace.NewTracerProvider(trace.WithSampler(trace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	ctx, span := StartClient(t.Context(), CallOptions{
		Method:   MethodToolsCall,
		ToolName: "search-web",
	})
	defer span.End()

	sc := traceapi.SpanContextFromContext(ctx)
	assert.True(t, sc.IsValid(), "context should carry an active span")
}

func TestClassifyError(t *testing.T) {
	t.Parallel()
	assert.Empty(t, ClassifyError(nil))
	assert.Equal(t, "context_canceled", ClassifyError(context.Canceled))
	assert.Equal(t, "deadline_exceeded", ClassifyError(context.DeadlineExceeded))
	assert.Equal(t, "rpc_error", ClassifyError(errors.New("some other error")))
}
