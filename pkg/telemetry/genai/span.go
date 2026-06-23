package genai

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ChatRequest carries the inputs needed to start a `chat {model}` span and
// to record the matching client metrics. Provider-specific extensions
// (openai service tier, aws.bedrock guardrail, etc.) attach via
// ChatSpan.SetAttributes after the span has started.
type ChatRequest struct {
	// Provider is the GenAI provider name. Use one of the Provider*
	// constants. Set on the span at creation time per the per-provider
	// semconv MUST clauses.
	Provider string

	// Model is the requested model identifier. Empty model is allowed
	// (some routers do not commit until inside the call) but produces a
	// span name of just "chat".
	Model string

	// Stream is true if the request is streaming. Recorded as
	// gen_ai.request.stream.
	Stream bool

	// ServerAddress / ServerPort identify the GenAI endpoint when known
	// (helpful for routing-aware dashboards). Optional.
	ServerAddress string
	ServerPort    int

	// Sampling parameters. Zero values are treated as unset and not
	// recorded on the span.
	MaxTokens        int
	Temperature      float64
	TopP             float64
	TopK             float64
	FrequencyPenalty float64
	PresencePenalty  float64
	Seed             int
	StopSequences    []string
	ChoiceCount      int

	// HasTemperature / HasTopP / HasTopK / HasFreqPenalty / HasPresPenalty
	// disambiguate "explicitly zero" from "unset" for the float params.
	// Callers that use the zero value as meaningful must set these.
	HasTemperature bool
	HasTopP        bool
	HasTopK        bool
	HasFreqPenalty bool
	HasPresPenalty bool
}

// ServerAddressFromURL extracts host and port for the ServerAddress /
// ServerPort fields when callers have a full URL handy.
func ServerAddressFromURL(raw string) (string, int) {
	if raw == "" {
		return "", 0
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", 0
	}
	port, _ := strconv.Atoi(u.Port())
	return u.Hostname(), port
}

// ChatSpan is the handle returned by StartChat. It wraps an OTel span and
// captures enough state to emit per-operation metrics on End.
type ChatSpan struct {
	span      trace.Span
	provider  string
	model     string
	startedAt time.Time
	// metricCtx carries the request context captured at StartChat
	// time so metric Record / Add calls in End preserve the
	// trace-to-metric exemplar link. Using context.Background() here
	// would silently strip the active span context and break
	// drill-from-metric-bucket-to-trace navigation in Tempo/Mimir.
	metricCtx context.Context //nolint:containedctx // intentional: needed for OTel exemplar attribution at End time

	mu            sync.Mutex
	ended         bool
	responseModel string
	finishReasons []string
	usageRecorded bool
	usage         chatUsage
	errType       string

	// Streaming metrics: the first non-empty chunk timestamp and the
	// previous chunk timestamp drive the time_to_first_chunk and
	// time_per_output_chunk histograms.
	firstChunkAt   time.Time
	prevChunkAt    time.Time
	chunkDurations []float64
}

type chatUsage struct {
	inputTokens        int64
	outputTokens       int64
	cacheReadInput     int64
	cacheCreationInput int64
	reasoningOutput    int64
}

// StartChat begins a CLIENT-kind `chat {model}` span and records the
// required gen_ai.* request attributes. The returned context carries the
// new span; callers MUST call ChatSpan.End to flush the span and metrics.
func StartChat(ctx context.Context, req ChatRequest) (context.Context, *ChatSpan) {
	tracer := otel.Tracer(instrumentationName)

	name := OperationChat
	if req.Model != "" {
		name = OperationChat + " " + req.Model
	}

	attrs := []attribute.KeyValue{
		attribute.String(AttrOperationName, OperationChat),
		attribute.String(AttrProviderName, req.Provider),
		attribute.Bool(AttrRequestStream, req.Stream),
	}
	if req.Model != "" {
		attrs = append(attrs, attribute.String(AttrRequestModel, req.Model))
	}
	if req.ServerAddress != "" {
		attrs = append(attrs, attribute.String("server.address", req.ServerAddress))
		if req.ServerPort > 0 {
			attrs = append(attrs, attribute.Int("server.port", req.ServerPort))
		}
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int(AttrRequestMaxTokens, req.MaxTokens))
	}
	if req.HasTemperature {
		attrs = append(attrs, attribute.Float64(AttrRequestTemperature, req.Temperature))
	}
	if req.HasTopP {
		attrs = append(attrs, attribute.Float64(AttrRequestTopP, req.TopP))
	}
	if req.HasTopK {
		attrs = append(attrs, attribute.Float64(AttrRequestTopK, req.TopK))
	}
	if req.HasFreqPenalty {
		attrs = append(attrs, attribute.Float64(AttrRequestFrequencyPenalty, req.FrequencyPenalty))
	}
	if req.HasPresPenalty {
		attrs = append(attrs, attribute.Float64(AttrRequestPresencePenalty, req.PresencePenalty))
	}
	if req.Seed != 0 {
		attrs = append(attrs, attribute.Int(AttrRequestSeed, req.Seed))
	}
	if len(req.StopSequences) > 0 {
		attrs = append(attrs, attribute.StringSlice(AttrRequestStopSequences, req.StopSequences))
	}
	if req.ChoiceCount > 0 && req.ChoiceCount != 1 {
		attrs = append(attrs, attribute.Int(AttrRequestChoiceCount, req.ChoiceCount))
	}
	if conv, ok := conversationAttribute(ctx); ok {
		attrs = append(attrs, conv)
	}

	ctx, span := tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)

	return ctx, &ChatSpan{
		span:      span,
		provider:  req.Provider,
		model:     req.Model,
		startedAt: time.Now(),
		metricCtx: ctx,
	}
}

// SetAttributes adds extra attributes to the span. Use for provider-specific
// fields (openai.*, aws.bedrock.*) and for response-side attributes the
// caller learns later.
func (s *ChatSpan) SetAttributes(attrs ...attribute.KeyValue) {
	if s == nil {
		return
	}
	s.span.SetAttributes(attrs...)
}

// SetResponseModel records gen_ai.response.model. Some providers return a
// resolved model name that differs from the requested one (alias expansion,
// version pinning); both values are useful.
func (s *ChatSpan) SetResponseModel(model string) {
	if s == nil || model == "" {
		return
	}
	s.mu.Lock()
	s.responseModel = model
	s.mu.Unlock()
	s.span.SetAttributes(attribute.String(AttrResponseModel, model))
}

// SetResponseID records gen_ai.response.id.
func (s *ChatSpan) SetResponseID(id string) {
	if s == nil || id == "" {
		return
	}
	s.span.SetAttributes(attribute.String(AttrResponseID, id))
}

// AddFinishReason accumulates a finish reason. The spec defines the
// attribute as a string array — multiple values are recorded once on End.
func (s *ChatSpan) AddFinishReason(reason string) {
	if s == nil || reason == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.finishReasons, reason) {
		return
	}
	s.finishReasons = append(s.finishReasons, reason)
}

// RecordUsage stores the token usage for emission as both span attributes
// and the gen_ai.client.token.usage histogram. Callers pass raw provider
// values; this package applies the spec-mandated Anthropic input-token sum
// (`input_tokens` reported by Anthropic excludes cached tokens, so the
// spec requires summing input + cache_read + cache_creation).
func (s *ChatSpan) RecordUsage(inputTokens, outputTokens, cacheReadInput, cacheCreationInput, reasoningOutput int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.inputTokens = inputTokens
	s.usage.outputTokens = outputTokens
	s.usage.cacheReadInput = cacheReadInput
	s.usage.cacheCreationInput = cacheCreationInput
	s.usage.reasoningOutput = reasoningOutput
	s.usageRecorded = true
}

// MarkChunk records the timing of a streamed output chunk. The first call
// drives gen_ai.response.time_to_first_chunk (and the corresponding
// metric); subsequent calls accumulate per-chunk durations.
func (s *ChatSpan) MarkChunk() {
	if s == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstChunkAt.IsZero() {
		s.firstChunkAt = now
	} else {
		s.chunkDurations = append(s.chunkDurations, now.Sub(s.prevChunkAt).Seconds())
	}
	s.prevChunkAt = now
}

// RecordError marks the span as failed and stores error.type for the
// duration metric. errType should be a short, low-cardinality string —
// "rate_limit", "context_length_exceeded", "auth", "network",
// "context_canceled", or "_OTHER" as the spec-defined fallback. When
// errType is empty, ClassifyError(err) is called to derive a value, so
// callers that don't already have a classification can pass "" without
// losing it to the "_OTHER" bucket.
func (s *ChatSpan) RecordError(err error, errType string) {
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

// End closes the span, flushes accumulated finish reasons / usage / timing
// to the span, and records the duration and token-usage histograms. Safe
// to call multiple times; subsequent calls are no-ops.
func (s *ChatSpan) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	finishReasons := append([]string(nil), s.finishReasons...)
	usage := s.usage
	usageRecorded := s.usageRecorded
	errType := s.errType
	firstChunkAt := s.firstChunkAt
	chunkDurations := append([]float64(nil), s.chunkDurations...)
	s.mu.Unlock()

	if len(finishReasons) > 0 {
		s.span.SetAttributes(attribute.StringSlice(AttrResponseFinishReasons, finishReasons))
	}
	if !firstChunkAt.IsZero() {
		ttfc := firstChunkAt.Sub(s.startedAt).Seconds()
		s.span.SetAttributes(attribute.Float64(AttrResponseTimeToFirstChunk, ttfc))
	}
	if usageRecorded {
		// Apply the spec-mandated Anthropic input-token math: Anthropic's
		// API reports input_tokens excluding cache, but spec wants the
		// inclusive total on gen_ai.usage.input_tokens.
		spanInputTokens := usage.inputTokens
		if s.provider == ProviderAnthropic {
			spanInputTokens += usage.cacheReadInput + usage.cacheCreationInput
		}
		spanAttrs := []attribute.KeyValue{
			attribute.Int64(AttrUsageInputTokens, spanInputTokens),
			attribute.Int64(AttrUsageOutputTokens, usage.outputTokens),
		}
		if usage.cacheReadInput > 0 {
			spanAttrs = append(spanAttrs, attribute.Int64(AttrUsageCacheReadInputTokens, usage.cacheReadInput))
		}
		if usage.cacheCreationInput > 0 {
			spanAttrs = append(spanAttrs, attribute.Int64(AttrUsageCacheCreationInputTokens, usage.cacheCreationInput))
		}
		if usage.reasoningOutput > 0 {
			spanAttrs = append(spanAttrs, attribute.Int64(AttrUsageReasoningOutputTokens, usage.reasoningOutput))
		}
		s.span.SetAttributes(spanAttrs...)
	}

	s.span.End()

	// Emit metrics. Failure to resolve instruments must not block span
	// completion, so we silently skip when getInstruments returns nil.
	insts := getInstruments()
	if insts == nil {
		return
	}

	commonAttrs := []attribute.KeyValue{
		attribute.String(AttrOperationName, OperationChat),
		attribute.String(AttrProviderName, s.provider),
	}
	// `gen_ai.request.model` is required here by the OTel GenAI
	// semconv but is unbounded in practice — every dated variant
	// (e.g. `model-YYYYMMDD`) opens a new metric series. Operators
	// concerned about backend cardinality should drop or canonicalise
	// this label at the collector rather than at the agent, so spans
	// keep full detail while metrics stay bounded.
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

	if !firstChunkAt.IsZero() && insts.clientOperationTTFC != nil {
		insts.clientOperationTTFC.Record(s.metricCtx, firstChunkAt.Sub(s.startedAt).Seconds(),
			metric.WithAttributes(commonAttrs...),
		)
	}
	if insts.clientOperationTimePerChunk != nil {
		for _, d := range chunkDurations {
			insts.clientOperationTimePerChunk.Record(s.metricCtx, d,
				metric.WithAttributes(commonAttrs...),
			)
		}
	}

	if usageRecorded && insts.clientTokenUsage != nil {
		recordTokenMetric := func(tokenType string, value int64) {
			if value <= 0 {
				return
			}
			tokenAttrs := append([]attribute.KeyValue(nil), commonAttrs...)
			tokenAttrs = append(tokenAttrs, attribute.String(AttrTokenType, tokenType))
			insts.clientTokenUsage.Record(s.metricCtx, value,
				metric.WithAttributes(tokenAttrs...),
			)
		}
		// Per-token-type metric data points use raw provider values so a
		// backend summing across types reconstructs the true total
		// without double-counting cached tokens. The Anthropic spec sum
		// (input + cache_read + cache_creation) is only applied to the
		// span attribute `gen_ai.usage.input_tokens` per the per-provider
		// semconv MUST clause — see span attribute emission above.
		recordTokenMetric(TokenTypeInput, usage.inputTokens)
		recordTokenMetric(TokenTypeOutput, usage.outputTokens)
		recordTokenMetric(TokenTypeCacheRead, usage.cacheReadInput)
		recordTokenMetric(TokenTypeCacheCreation, usage.cacheCreationInput)
		recordTokenMetric(TokenTypeReasoning, usage.reasoningOutput)
	}
}

// Span returns the underlying OTel span so callers can attach span events
// or links when they need finer control than the helpers expose. Returns
// a real no-op span (not a struct embedding a nil trace.Span) when the
// receiver is nil so callers don't have to nil-check before invoking
// Span methods like AddEvent / SetAttributes.
func (s *ChatSpan) Span() trace.Span {
	if s == nil {
		return tracenoop.Span{}
	}
	return s.span
}
