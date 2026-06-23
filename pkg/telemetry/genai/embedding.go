package genai

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// EmbeddingRequest carries the inputs needed to start an
// `embeddings {model}` span per the OTel GenAI semantic conventions.
type EmbeddingRequest struct {
	Provider string
	Model    string
	// BatchSize is the number of input texts in the embedding call,
	// recorded as `cagent.embeddings.batch_size`. Zero means
	// single-input.
	BatchSize int
	// EncodingFormats is the optional list of requested output
	// encodings (e.g. "float", "base64") per the GenAI semconv.
	// Recorded as `gen_ai.request.encoding_formats` when non-empty.
	EncodingFormats []string
}

// EmbeddingSpan handles the lifecycle of an embedding span and the
// matching `gen_ai.client.operation.duration` / `gen_ai.client.token.usage`
// metric records.
type EmbeddingSpan struct {
	span      trace.Span
	provider  string
	model     string
	startedAt time.Time
	metricCtx context.Context //nolint:containedctx // intentional: needed for OTel exemplar attribution at End time

	mu          sync.Mutex
	ended       bool
	inputTokens int64
	dimensions  int
	errType     string
}

// StartEmbedding begins a CLIENT-kind `embeddings {model}` span and
// records the spec-required `gen_ai.operation.name=embeddings`,
// `gen_ai.provider.name`, and `gen_ai.request.model` attributes.
func StartEmbedding(ctx context.Context, req EmbeddingRequest) (context.Context, *EmbeddingSpan) {
	tracer := otel.Tracer(instrumentationName)
	name := OperationEmbeddings
	if req.Model != "" {
		name = OperationEmbeddings + " " + req.Model
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrOperationName, OperationEmbeddings),
		attribute.String(AttrProviderName, req.Provider),
	}
	if req.Model != "" {
		attrs = append(attrs, attribute.String(AttrRequestModel, req.Model))
	}
	if req.BatchSize > 1 {
		attrs = append(attrs, attribute.Int("cagent.embeddings.batch_size", req.BatchSize))
	}
	if len(req.EncodingFormats) > 0 {
		attrs = append(attrs, attribute.StringSlice(AttrRequestEncodingFormats, req.EncodingFormats))
	}
	if conv, ok := conversationAttribute(ctx); ok {
		attrs = append(attrs, conv)
	}
	ctx, span := tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return ctx, &EmbeddingSpan{
		span:      span,
		provider:  req.Provider,
		model:     req.Model,
		startedAt: time.Now(),
		metricCtx: ctx,
	}
}

// SetInputTokens records the number of input tokens consumed by the
// embedding call. Emitted as `gen_ai.usage.input_tokens` on the span
// and as the `gen_ai.client.token.usage` metric at End time.
func (s *EmbeddingSpan) SetInputTokens(n int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.inputTokens = n
	s.mu.Unlock()
}

// SetDimensions records the dimensionality of the resulting embedding
// vector(s). Emitted as `gen_ai.embeddings.dimension.count`.
func (s *EmbeddingSpan) SetDimensions(d int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dimensions = d
	s.mu.Unlock()
}

// RecordError marks the span as failed and stores `error.type` for the
// duration metric.
func (s *EmbeddingSpan) RecordError(err error, errType string) {
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

// End closes the span and records the duration + token-usage metrics.
func (s *EmbeddingSpan) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	inputTokens := s.inputTokens
	dimensions := s.dimensions
	errType := s.errType
	s.mu.Unlock()

	if inputTokens > 0 {
		s.span.SetAttributes(attribute.Int64(AttrUsageInputTokens, inputTokens))
	}
	if dimensions > 0 {
		s.span.SetAttributes(attribute.Int(AttrEmbeddingsDimensionCount, dimensions))
	}
	s.span.End()

	insts := getInstruments()
	if insts == nil {
		return
	}
	commonAttrs := []attribute.KeyValue{
		attribute.String(AttrOperationName, OperationEmbeddings),
		attribute.String(AttrProviderName, s.provider),
	}
	if s.model != "" {
		commonAttrs = append(commonAttrs, attribute.String(AttrRequestModel, s.model))
	}
	durationAttrs := append([]attribute.KeyValue(nil), commonAttrs...)
	if errType != "" {
		durationAttrs = append(durationAttrs, attribute.String("error.type", errType))
	}
	if insts.clientOperationDuration != nil {
		insts.clientOperationDuration.Record(s.metricCtx, time.Since(s.startedAt).Seconds(),
			metric.WithAttributes(durationAttrs...),
		)
	}
	if inputTokens > 0 && insts.clientTokenUsage != nil {
		tokenAttrs := append([]attribute.KeyValue(nil), commonAttrs...)
		tokenAttrs = append(tokenAttrs, attribute.String(AttrTokenType, TokenTypeInput))
		insts.clientTokenUsage.Record(s.metricCtx, inputTokens,
			metric.WithAttributes(tokenAttrs...),
		)
	}
}
