package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// CallOptions describes an MCP request being made or handled. Used by
// both client- and server-side helpers so call sites depend on a single
// vocabulary.
type CallOptions struct {
	// Method is the MCP method name (e.g. "tools/call"). Required.
	Method string

	// Target is the low-cardinality target of the operation: tool name
	// for tools/call, prompt name for prompts/get, etc. When set the
	// span name becomes "{method} {target}"; otherwise just "{method}".
	Target string

	// ToolName, when set, is recorded as gen_ai.tool.name and used as
	// the default Target for tools/call.
	ToolName string

	// PromptName, when set, is recorded as gen_ai.prompt.name and used
	// as the default Target for prompts/get.
	PromptName string

	// ResourceURI, when set, is recorded as mcp.resource.uri and used
	// as the default Target for resources/* methods.
	ResourceURI string

	// SessionID identifies the MCP session and is recorded as
	// mcp.session.id when set.
	SessionID string

	// ProtocolVersion is recorded as mcp.protocol.version when set.
	ProtocolVersion string

	// JSONRPCRequestID is recorded as jsonrpc.request.id when set
	// (client-side requests; ignored for notifications).
	JSONRPCRequestID string

	// ServerAddress / ServerPort identify the MCP endpoint when known.
	ServerAddress string
	ServerPort    int
}

// Span is the handle returned by StartClient / StartServer. It carries
// enough state to record `mcp.{client,server}.operation.duration` and to
// flush span attributes as the operation proceeds.
type Span struct {
	span trace.Span
	// metricCtx carries the active span context so the duration
	// histogram measurement produces span-context exemplars (drill
	// Mimir bucket → Tempo trace).
	metricCtx context.Context //nolint:containedctx // intentional: needed for OTel exemplar attribution at End time
	startedAt time.Time
	method    string
	kind      trace.SpanKind

	mu      sync.Mutex
	errType string
	ended   bool
}

// StartClient begins a CLIENT-kind MCP span and returns a context carrying
// it. Callers MUST call Span.End to flush the span and metrics.
func StartClient(ctx context.Context, opts CallOptions) (context.Context, *Span) {
	return startSpan(ctx, opts, trace.SpanKindClient)
}

// StartServer begins a SERVER-kind MCP span. Use after extracting trace
// context from the incoming `params._meta` so the span chains onto the
// caller. Callers MUST call Span.End.
func StartServer(ctx context.Context, opts CallOptions) (context.Context, *Span) {
	return startSpan(ctx, opts, trace.SpanKindServer)
}

func startSpan(ctx context.Context, opts CallOptions, kind trace.SpanKind) (context.Context, *Span) {
	tracer := otel.Tracer(instrumentationName)

	target := opts.Target
	if target == "" {
		switch {
		case opts.ToolName != "":
			target = opts.ToolName
		case opts.PromptName != "":
			target = opts.PromptName
		case opts.ResourceURI != "":
			target = opts.ResourceURI
		}
	}

	name := opts.Method
	if name == "" {
		name = "mcp"
	}
	if target != "" {
		name = name + " " + target
	}

	attrs := []attribute.KeyValue{
		attribute.String(AttrMethodName, opts.Method),
	}
	if opts.ToolName != "" {
		attrs = append(attrs,
			attribute.String(AttrGenAIToolName, opts.ToolName),
		)
		if strings.HasPrefix(opts.Method, "tools/") {
			attrs = append(attrs, attribute.String(AttrGenAIOperationName, OperationExecuteTool))
		}
	}
	if opts.PromptName != "" {
		attrs = append(attrs, attribute.String(AttrGenAIPromptName, opts.PromptName))
	}
	if opts.ResourceURI != "" {
		attrs = append(attrs, attribute.String(AttrResourceURI, opts.ResourceURI))
	}
	if opts.SessionID != "" {
		attrs = append(attrs, attribute.String(AttrSessionID, opts.SessionID))
	}
	if opts.ProtocolVersion != "" {
		attrs = append(attrs, attribute.String(AttrProtocolVersion, opts.ProtocolVersion))
	}
	if opts.JSONRPCRequestID != "" {
		attrs = append(attrs, attribute.String(AttrJSONRPCRequestID, opts.JSONRPCRequestID))
	}
	if opts.ServerAddress != "" {
		attrs = append(attrs, attribute.String("server.address", opts.ServerAddress))
		if opts.ServerPort > 0 {
			attrs = append(attrs, attribute.Int("server.port", opts.ServerPort))
		}
	}
	if conv := ConversationIDFromBaggage(ctx); conv != "" {
		attrs = append(attrs, attribute.String("gen_ai.conversation.id", conv))
	}

	ctx, span := tracer.Start(ctx, name,
		trace.WithSpanKind(kind),
		trace.WithAttributes(attrs...),
	)

	return ctx, &Span{
		span:      span,
		metricCtx: ctx,
		startedAt: time.Now(),
		method:    opts.Method,
		kind:      kind,
	}
}

// SetAttributes adds extra attributes to the span. Use for MCP extensions
// or for response-side attributes the caller learns later
// (e.g. rpc.response.status_code).
func (s *Span) SetAttributes(attrs ...attribute.KeyValue) {
	if s == nil {
		return
	}
	s.span.SetAttributes(attrs...)
}

// RecordError marks the span as failed and stores error.type for the
// duration metric. errType should be a short, low-cardinality string;
// when empty, ClassifyError(err) supplies a value (one of
// "context_canceled", "deadline_exceeded", "rpc_error").
func (s *Span) RecordError(err error, errType string) {
	if s == nil || err == nil {
		return
	}
	if errType == "" {
		errType = ClassifyError(err)
	}
	s.mu.Lock()
	s.errType = errType
	s.mu.Unlock()
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
	s.span.SetAttributes(attribute.String("error.type", errType))
}

// End closes the span and records the operation duration metric. Safe to
// call multiple times; subsequent calls are no-ops.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	errType := s.errType
	s.mu.Unlock()

	s.span.End()

	insts := getInstruments()
	if insts == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrMethodName, s.method),
	}
	if errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}

	histogram := insts.clientOperationDuration
	if s.kind == trace.SpanKindServer {
		histogram = insts.serverOperationDuration
	}
	if histogram == nil {
		return
	}
	// Use the span's started-at as the reference; we already snapshot
	// errType under the lock above, so no additional locking is needed
	// for the immutable startedAt field.
	histogram.Record(s.metricCtx, time.Since(s.startedAt).Seconds(),
		metric.WithAttributes(attrs...),
	)
}

// ClassifyError maps an MCP error to a low-cardinality error.type value.
// MCP errors are often plain RPC errors; this helper picks reasonable
// labels for cancellation and falls back to the type name otherwise.
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	return "rpc_error"
}
