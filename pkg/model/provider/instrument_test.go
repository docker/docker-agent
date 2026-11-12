package provider

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

type instrumentTestProvider struct {
	base.Config

	events    []string
	onCall    func(context.Context)
	err       error
	stream    *instrumentTestStream
	embedding *base.EmbeddingResult
	batch     *base.BatchEmbeddingResult
	scores    []float64
	messages  []chat.Message
	tools     []tools.Tool
	text      string
	texts     []string
	query     string
	documents []types.Document
	criteria  string
}

func (p *instrumentTestProvider) BaseConfig() base.Config {
	p.events = append(p.events, "config")
	return p.Config
}

func (p *instrumentTestProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, requestTools []tools.Tool) (chat.MessageStream, error) {
	p.events = append(p.events, "chat")
	p.onCall(ctx)
	p.messages, p.tools = messages, requestTools
	return p.stream, p.err
}

func (p *instrumentTestProvider) CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error) {
	p.events = append(p.events, "embed")
	p.onCall(ctx)
	p.text = text
	return p.embedding, p.err
}

func (p *instrumentTestProvider) CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error) {
	p.events = append(p.events, "batch")
	p.onCall(ctx)
	p.texts = texts
	return p.batch, p.err
}

func (p *instrumentTestProvider) Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error) {
	p.events = append(p.events, "rerank")
	p.onCall(ctx)
	p.query, p.documents, p.criteria = query, documents, criteria
	return p.scores, p.err
}

type instrumentTestStream struct {
	received int
	closed   int
}

func (s *instrumentTestStream) Recv() (chat.MessageStreamResponse, error) {
	s.received++
	return chat.MessageStreamResponse{}, io.EOF
}

func (s *instrumentTestStream) Close() { s.closed++ }

type instrumentTestEmbedReranker interface {
	EmbeddingProvider
	RerankingProvider
}

// Serial: the recording tracer replaces global OTel state.
func TestInstrumentProvider(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		require.NoError(t, tp.Shutdown(context.WithoutCancel(t.Context())))
	})

	for _, tc := range []struct {
		name   string
		narrow func(*instrumentTestProvider) Provider
		embed  bool
		batch  bool
		rerank bool
	}{
		{name: "chat", narrow: func(p *instrumentTestProvider) Provider { return &struct{ Provider }{p} }},
		{name: "rerank", rerank: true, narrow: func(p *instrumentTestProvider) Provider { return &struct{ RerankingProvider }{p} }},
		{name: "embed", embed: true, narrow: func(p *instrumentTestProvider) Provider { return &struct{ EmbeddingProvider }{p} }},
		{name: "embed rerank", embed: true, rerank: true, narrow: func(p *instrumentTestProvider) Provider { return &struct{ instrumentTestEmbedReranker }{p} }},
		{name: "batch", embed: true, batch: true, narrow: func(p *instrumentTestProvider) Provider { return &struct{ BatchEmbeddingProvider }{p} }},
		{name: "batch rerank", embed: true, batch: true, rerank: true, narrow: func(p *instrumentTestProvider) Provider { return p }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, outcome := range []string{"success", "nil non-streaming results", "empty non-streaming results", "error with result"} {
				t.Run(outcome, func(t *testing.T) {
					exporter.Reset()
					ctx := genai.WithConversationID(t.Context(), "conversation")
					ctx, parent := tp.Tracer("test").Start(ctx, "parent")
					defer parent.End()
					p := &instrumentTestProvider{
						Config: base.Config{ModelConfig: latest.ModelConfig{
							Provider: "openai", Model: "before-wrapping",
							MaxTokens: new(int64(42)), Temperature: new(0.0), TopP: new(0.8),
						}},
						stream:    &instrumentTestStream{},
						embedding: &base.EmbeddingResult{Embedding: []float64{0.1, 0.2}, InputTokens: 7},
						batch:     &base.BatchEmbeddingResult{Embeddings: [][]float64{{0.1, 0.2}, {0.3, 0.4}}, InputTokens: 11},
						scores:    []float64{0.4, 0.9},
					}
					switch outcome {
					case "nil non-streaming results":
						p.embedding, p.batch, p.scores = nil, nil, nil
					case "empty non-streaming results":
						p.embedding = &base.EmbeddingResult{}
						p.batch = &base.BatchEmbeddingResult{}
						p.scores = []float64{}
					case "error with result":
						p.err = context.DeadlineExceeded
					}
					inner := tc.narrow(p)
					wrapped := instrumentProvider(inner)
					assert.Empty(t, p.events, "construction must not query the provider")
					assert.Empty(t, exporter.GetSpans())
					p.ModelConfig.Model = "live-model"
					assert.Equal(t, p.ID(), wrapped.ID())
					assert.Equal(t, p.Config, wrapped.BaseConfig())
					assert.Equal(t, []string{"config"}, p.events)
					assert.Same(t, inner, wrapped.(interface{ Unwrap() Provider }).Unwrap())
					assert.Same(t, inner, unwrapProvider(instrumentProvider(wrapped)))

					ep, embeds := wrapped.(EmbeddingProvider)
					bp, batches := wrapped.(BatchEmbeddingProvider)
					rp, reranks := wrapped.(RerankingProvider)
					require.Equal(t, tc.embed, embeds)
					require.Equal(t, tc.batch, batches)
					require.Equal(t, tc.rerank, reranks)

					var active trace.SpanContext
					before := 0
					p.onCall = func(callCtx context.Context) {
						span := trace.SpanFromContext(callCtx)
						assert.True(t, span.IsRecording(), "span must start before delegation")
						active = span.SpanContext()
						assert.NotEqual(t, parent.SpanContext().SpanID(), active.SpanID())
						assert.Equal(t, "conversation", genai.ConversationIDFromContext(callCtx))
						assert.Len(t, exporter.GetSpans(), before, "span must remain open during delegation")
					}
					checkSpan := func(operation, name string, attrs ...attribute.KeyValue) {
						t.Helper()
						spans := exporter.GetSpans()
						require.Len(t, spans, before+1, "one span per operation")
						span := spans[before]
						assert.Equal(t, name, span.Name)
						assert.Equal(t, trace.SpanKindClient, span.SpanKind)
						assert.Equal(t, active, span.SpanContext)
						assert.Equal(t, parent.SpanContext(), span.Parent)
						assert.Contains(t, span.Attributes, attribute.String(genai.AttrProviderName, "openai"))
						assert.Contains(t, span.Attributes, attribute.String(genai.AttrRequestModel, "live-model"))
						assert.Contains(t, span.Attributes, attribute.String(genai.AttrConversationID, "conversation"))
						for _, attr := range attrs {
							assert.Contains(t, span.Attributes, attr)
						}
						if (operation == "embed" || operation == "batch") && outcome != "success" {
							for _, attr := range span.Attributes {
								assert.NotEqual(t, attribute.Key(genai.AttrUsageInputTokens), attr.Key)
								assert.NotEqual(t, attribute.Key(genai.AttrEmbeddingsDimensionCount), attr.Key)
							}
						}
						if p.err != nil {
							assert.Equal(t, codes.Error, span.Status.Code)
							assert.Equal(t, p.err.Error(), span.Status.Description)
							assert.Contains(t, span.Attributes, attribute.String("error.type", "deadline_exceeded"))
							require.Len(t, span.Events, 1)
							assert.Equal(t, "exception", span.Events[0].Name)
						} else {
							assert.NotEqual(t, codes.Error, span.Status.Code)
							assert.Empty(t, span.Events)
						}
						assert.Equal(t, []string{"config", operation}, p.events, "configuration lookup and delegation must each happen once, in order")
						p.events = nil
						before++
					}

					p.events = nil
					messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}
					requestTools := []tools.Tool{{Name: "lookup"}}
					stream, err := wrapped.CreateChatCompletionStream(ctx, messages, requestTools)
					if p.err != nil {
						require.ErrorIs(t, err, p.err)
						assert.Nil(t, stream)
						assert.Zero(t, p.stream.closed)
					} else {
						require.NoError(t, err)
						assert.Empty(t, exporter.GetSpans(), "chat span stays open until the stream is closed")
						_, err = stream.Recv()
						require.ErrorIs(t, err, io.EOF)
						assert.Empty(t, exporter.GetSpans())
						stream.Close()
						stream.Close()
						assert.Equal(t, 1, p.stream.received)
						assert.Equal(t, 1, p.stream.closed)
					}
					assert.Equal(t, messages, p.messages)
					assert.Equal(t, requestTools, p.tools)
					checkSpan("chat", "chat live-model",
						attribute.String(genai.AttrOperationName, "chat"),
						attribute.Int(genai.AttrRequestMaxTokens, 42),
						attribute.Float64(genai.AttrRequestTemperature, 0),
						attribute.Float64(genai.AttrRequestTopP, 0.8))

					if embeds {
						result, err := ep.CreateEmbedding(ctx, "input")
						if p.err != nil {
							require.ErrorIs(t, err, p.err)
							assert.Nil(t, result)
						} else {
							require.NoError(t, err)
							assert.Same(t, p.embedding, result)
						}
						assert.Equal(t, "input", p.text)
						var attrs []attribute.KeyValue
						if outcome == "success" {
							attrs = append(attrs, attribute.Int64(genai.AttrUsageInputTokens, 7), attribute.Int(genai.AttrEmbeddingsDimensionCount, 2))
						}
						checkSpan("embed", "embeddings live-model", attrs...)
					}
					if batches {
						texts := []string{"first", "second"}
						result, err := bp.CreateBatchEmbedding(ctx, texts)
						if p.err != nil {
							require.ErrorIs(t, err, p.err)
							assert.Nil(t, result)
						} else {
							require.NoError(t, err)
							assert.Same(t, p.batch, result)
						}
						assert.Equal(t, texts, p.texts)
						attrs := []attribute.KeyValue{attribute.Int("cagent.embeddings.batch_size", 2)}
						if outcome == "success" {
							attrs = append(attrs, attribute.Int64(genai.AttrUsageInputTokens, 11), attribute.Int(genai.AttrEmbeddingsDimensionCount, 2))
						}
						checkSpan("batch", "embeddings live-model", attrs...)
					}
					if reranks {
						documents := []types.Document{{Content: "first"}, {Content: "second"}}
						scores, err := rp.Rerank(ctx, "query", documents, "criteria")
						if p.err != nil {
							require.ErrorIs(t, err, p.err)
							assert.Nil(t, scores)
						} else {
							require.NoError(t, err)
							assert.Equal(t, p.scores, scores)
							if len(scores) > 0 {
								assert.Same(t, &p.scores[0], &scores[0])
							}
						}
						assert.Equal(t, "query", p.query)
						assert.Equal(t, documents, p.documents)
						assert.Equal(t, "criteria", p.criteria)
						checkSpan("rerank", "rerank", attribute.Int("cagent.rerank.document_count", 2))
					}
				})
			}
		})
	}
}

func TestInstrumentProviderNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, instrumentProvider(nil))
}

func TestInstrumentProviderForwardsRebuilder(t *testing.T) {
	t.Parallel()
	p := &instrumentTestProvider{}
	wrapped := instrumentProvider(p)
	calls := 0
	p.SetProviderRebuilder(func(_ context.Context, _ *latest.ModelConfig, opts ...options.Opt) (Provider, error) {
		calls++
		modelOpts := options.Apply(opts...)
		assert.Equal(t, int64(123), modelOpts.MaxTokens())
		return p, nil
	})
	cfg := wrapped.BaseConfig()
	require.NotNil(t, cfg.RebuildProvider)
	clone, err := cfg.RebuildProvider(t.Context(), &cfg.ModelConfig, options.WithMaxTokens(123))
	require.NoError(t, err)
	assert.Same(t, p, clone)
	assert.Equal(t, 1, calls)
}

type instrumentContextProvider struct{ instrumentTestProvider }

func (*instrumentContextProvider) ContextWindow(context.Context) (int64, error) { return 8192, nil }

func TestContextWindowResolverThroughInstrumentation(t *testing.T) {
	t.Parallel()
	assert.Nil(t, ContextWindowResolver(nil))
	assert.Nil(t, ContextWindowResolver(instrumentProvider(&instrumentTestProvider{})))
	wrapped := instrumentProvider(&instrumentContextProvider{})
	resolver := ContextWindowResolver(wrapped)
	require.NotNil(t, resolver)
	n, err := resolver(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(8192), n)
}
