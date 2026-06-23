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

// Custom (non-spec) attribute keys for runtime-side observability that has
// no GenAI semconv equivalent yet (fallback chain, response cache,
// approval pipeline). Kept under the `cagent.` namespace so they are
// clearly distinguishable from the spec-defined `gen_ai.*` and `mcp.*`
// attributes when scrolling through a span.
const (
	AttrFallbackPrimaryModel = "cagent.fallback.primary_model"
	AttrFallbackFinalModel   = "cagent.fallback.final_model"
	AttrFallbackAttempts     = "cagent.fallback.attempts"
	AttrFallbackOutcome      = "cagent.fallback.outcome"
	AttrFallbackInCooldown   = "cagent.fallback.in_cooldown"

	AttrCacheHit     = "cagent.cache.hit"
	AttrCacheBacking = "cagent.cache.backing"

	AttrAgentNameRuntime = "cagent.agent.name"

	AttrRetrievalResultCount = "cagent.retrieval.result_count"

	AttrSandboxRuntime   = "cagent.sandbox.runtime"
	AttrSandboxImage     = "cagent.sandbox.image"
	AttrSandboxContainer = "cagent.sandbox.container"
	AttrSandboxExitCode  = "cagent.sandbox.exit_code"
)

// FallbackOutcome values for AttrFallbackOutcome.
const (
	FallbackOutcomeSuccess         = "success"
	FallbackOutcomeFailed          = "failed"
	FallbackOutcomeContextCanceled = "context_canceled"
)

// FallbackSpan is the handle for an in-flight runtime.fallback span.
type FallbackSpan struct {
	span      trace.Span
	startedAt time.Time

	mu       sync.Mutex
	attempts int
	final    string
	outcome  string
	errType  string
	ended    bool
}

// StartFallback begins a runtime.fallback span covering the whole fallback
// chain for one agent turn. Each per-model attempt produces its own
// `chat {model}` CLIENT child span (created by the provider decorator).
// Attributes set up front: primary model name, agent name, in-cooldown
// flag. The caller updates final model / attempts / outcome through the
// returned handle and calls End to flush.
func StartFallback(ctx context.Context, agentName, primaryModel string, inCooldown bool) (context.Context, *FallbackSpan) {
	tracer := otel.Tracer(instrumentationName)
	attrs := []attribute.KeyValue{
		attribute.String(AttrAgentNameRuntime, agentName),
		attribute.Bool(AttrFallbackInCooldown, inCooldown),
	}
	if primaryModel != "" {
		attrs = append(attrs, attribute.String(AttrFallbackPrimaryModel, primaryModel))
	}
	if conv, ok := conversationAttribute(ctx); ok {
		attrs = append(attrs, conv)
	}
	ctx, span := tracer.Start(ctx, "runtime.fallback",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, &FallbackSpan{
		span:      span,
		startedAt: time.Now(),
	}
}

// IncrementAttempt counts one attempt against the chain. Called once per
// (model × retry) iteration so the final span carries the total count.
func (s *FallbackSpan) IncrementAttempt() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
}

// SetFinalModel records the model that ultimately served the response.
// Called on the success path; not called on full-failure paths so the
// attribute remains absent and dashboards can distinguish the cases.
func (s *FallbackSpan) SetFinalModel(model string) {
	if s == nil || model == "" {
		return
	}
	s.mu.Lock()
	s.final = model
	s.mu.Unlock()
}

// RecordError stores an error and an error.type label for the metric.
func (s *FallbackSpan) RecordError(err error, errType string) {
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

// SetOutcome records the terminal outcome of the chain. Use one of the
// FallbackOutcome* constants.
func (s *FallbackSpan) SetOutcome(outcome string) {
	if s == nil || outcome == "" {
		return
	}
	s.mu.Lock()
	s.outcome = outcome
	s.mu.Unlock()
}

// End closes the span and flushes accumulated attributes.
func (s *FallbackSpan) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	final := s.final
	outcome := s.outcome
	attempts := s.attempts
	s.mu.Unlock()

	if final != "" {
		s.span.SetAttributes(attribute.String(AttrFallbackFinalModel, final))
	}
	if outcome != "" {
		s.span.SetAttributes(attribute.String(AttrFallbackOutcome, outcome))
	}
	s.span.SetAttributes(attribute.Int(AttrFallbackAttempts, attempts))
	s.span.End()
}

// RetrievalSpan handles a retrieval-operation span lifecycle.
type RetrievalSpan struct {
	span      trace.Span
	startedAt time.Time

	mu          sync.Mutex
	resultCount int
	errType     string
	ended       bool
}

// StartRetrieval begins a `retrieval {data_source.id}` span per the OTel
// GenAI semconv. providerName identifies the retrieval backend
// ("sqlite", "rag", an embedding-provider name) and is Required by the
// spec for retrieval operations. dataSourceID identifies the corpus /
// index / collection being queried; queryText is captured only when
// the caller has confirmed the content-capture opt-in.
func StartRetrieval(ctx context.Context, providerName, dataSourceID string, captureQuery bool, queryText string) (context.Context, *RetrievalSpan) {
	tracer := otel.Tracer(instrumentationName)
	name := OperationRetrieval
	if dataSourceID != "" {
		name = OperationRetrieval + " " + dataSourceID
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrOperationName, OperationRetrieval),
	}
	if providerName != "" {
		attrs = append(attrs, attribute.String(AttrProviderName, providerName))
	}
	if dataSourceID != "" {
		attrs = append(attrs, attribute.String(AttrDataSourceID, dataSourceID))
	}
	if captureQuery && queryText != "" {
		attrs = append(attrs, attribute.String(AttrRetrievalQueryText, queryText))
	}
	if conv, ok := conversationAttribute(ctx); ok {
		attrs = append(attrs, conv)
	}
	ctx, span := tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, &RetrievalSpan{span: span, startedAt: time.Now()}
}

// SetAttributes adds extra attributes to the retrieval span. Use for
// retrieval-specific extensions (corpus filter, category, fusion mode,
// etc.) that don't have a dedicated setter.
func (s *RetrievalSpan) SetAttributes(attrs ...attribute.KeyValue) {
	if s == nil {
		return
	}
	s.span.SetAttributes(attrs...)
}

// SetResultCount records how many documents the retrieval returned.
func (s *RetrievalSpan) SetResultCount(n int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.resultCount = n
	s.mu.Unlock()
}

// RecordError marks the retrieval span as failed.
func (s *RetrievalSpan) RecordError(err error, errType string) {
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

// End closes the retrieval span and flushes the result count.
func (s *RetrievalSpan) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	count := s.resultCount
	s.mu.Unlock()
	s.span.SetAttributes(attribute.Int(AttrRetrievalResultCount, count))
	s.span.End()
}

// CacheRequest counter — records every cache lookup with `result=hit|miss`
// and a `backing` attribute for memory-only vs file-backed caches.
var (
	cacheCounterOnce sync.Once
	cacheCounter     metric.Int64Counter
)

func getCacheCounter() metric.Int64Counter {
	cacheCounterOnce.Do(func() {
		meter := otel.Meter(instrumentationName)
		c, err := meter.Int64Counter(
			"cagent.cache.requests",
			metric.WithUnit("{request}"),
			metric.WithDescription("Number of response-cache lookups, broken down by hit/miss."),
		)
		if err != nil {
			return
		}
		cacheCounter = c
	})
	return cacheCounter
}

// RecordCacheLookup increments the cache counter and returns a small span
// describing the lookup. Callers `defer span.End()` and the helper sets
// `cagent.cache.hit` from the value returned by SetHit.
func RecordCacheLookup(ctx context.Context, backing string) (context.Context, *CacheSpan) {
	return startCacheSpan(ctx, "cache.lookup", "lookup", backing)
}

// RecordCacheStore is the Store-side counterpart of RecordCacheLookup.
func RecordCacheStore(ctx context.Context, backing string) (context.Context, *CacheSpan) {
	return startCacheSpan(ctx, "cache.store", "store", backing)
}

func startCacheSpan(ctx context.Context, spanName, op, backing string) (context.Context, *CacheSpan) {
	tracer := otel.Tracer(instrumentationName)
	attrs := []attribute.KeyValue{}
	if backing != "" {
		attrs = append(attrs, attribute.String(AttrCacheBacking, backing))
	}
	if conv, ok := conversationAttribute(ctx); ok {
		attrs = append(attrs, conv)
	}
	ctx, span := tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, &CacheSpan{span: span, metricCtx: ctx, backing: backing, op: op}
}

// CacheSpan handles cache-operation span lifecycle.
type CacheSpan struct {
	span trace.Span
	// metricCtx carries the active span context so counter Add calls
	// produce span-context exemplars (drill Mimir bucket → Tempo
	// trace). Without this the counter measurement gets only the
	// resource attributes.
	metricCtx context.Context //nolint:containedctx // intentional: needed for OTel exemplar attribution at End time
	backing   string
	op        string

	mu  sync.Mutex
	hit bool
	set bool
}

// SetHit records whether the lookup found an entry. Increments the
// cache counter immediately so the metric reflects the result even if End
// is called late.
func (s *CacheSpan) SetHit(hit bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.hit = hit
	s.set = true
	s.mu.Unlock()
	s.span.SetAttributes(attribute.Bool(AttrCacheHit, hit))

	if c := getCacheCounter(); c != nil {
		result := "miss"
		if hit {
			result = "hit"
		}
		attrs := []attribute.KeyValue{
			attribute.String("result", result),
			attribute.String("operation", s.op),
		}
		if s.backing != "" {
			attrs = append(attrs, attribute.String(AttrCacheBacking, s.backing))
		}
		// Use the active context so the counter measurement carries
		// the span exemplar — drill from Mimir bucket → Tempo trace
		// works for cache operations the same way it does for chat.
		c.Add(s.metricCtx, 1, metric.WithAttributes(attrs...))
	}
}

// End closes the cache span.
func (s *CacheSpan) End() {
	if s == nil {
		return
	}
	s.span.End()
}
