package genai

import (
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// instrumentationName identifies this package as the OTel instrumentation
// scope for spans, metrics, and log records it produces.
const instrumentationName = "github.com/docker/docker-agent/pkg/telemetry/genai"

// metricBucketsDuration matches the spec for `gen_ai.client.operation.duration`
// (and related per-chunk timing histograms).
var metricBucketsDuration = []float64{
	0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
}

// metricBucketsTokenUsage matches the spec for `gen_ai.client.token.usage`.
var metricBucketsTokenUsage = []float64{
	1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864,
}

// instruments holds the lazily-initialised metric instruments. Resolved on
// first use because the global MeterProvider is set at SDK init time, which
// may run after package-level var initialisation in some contexts.
type instruments struct {
	clientOperationDuration     metric.Float64Histogram
	clientOperationTTFC         metric.Float64Histogram
	clientOperationTimePerChunk metric.Float64Histogram
	clientTokenUsage            metric.Int64Histogram
}

var (
	instOnce sync.Once
	inst     *instruments
)

// getInstruments resolves and caches the package-level meter instruments.
// Histogram creation rarely fails in practice; when one does we keep the
// instruments that did succeed and leave the failed one nil. Call sites
// already nil-check each instrument, so a partial set is functional —
// previously a single early return left every metric permanently
// disabled, which surprised production debugging when one bucket
// configuration tripped a registration error.
//
// Test note: instruments are bound to the global MeterProvider on first
// call and frozen for the process lifetime via sync.Once. Replacing the
// provider with otel.SetMeterProvider after any production code path
// has already triggered getInstruments will NOT rebind the histograms,
// so tests that inspect emitted metrics must install their provider
// before any code under test runs (typically in TestMain or per-test
// setup before the first instrumented call).
func getInstruments() *instruments {
	instOnce.Do(func() {
		meter := otel.Meter(instrumentationName)
		i := &instruments{}

		i.clientOperationDuration, _ = meter.Float64Histogram(
			"gen_ai.client.operation.duration",
			metric.WithUnit("s"),
			metric.WithDescription("GenAI operation duration."),
			metric.WithExplicitBucketBoundaries(metricBucketsDuration...),
		)
		i.clientOperationTTFC, _ = meter.Float64Histogram(
			"gen_ai.client.operation.time_to_first_chunk",
			metric.WithUnit("s"),
			metric.WithDescription("Time to receive the first chunk of a streaming GenAI response."),
			metric.WithExplicitBucketBoundaries(metricBucketsDuration...),
		)
		i.clientOperationTimePerChunk, _ = meter.Float64Histogram(
			"gen_ai.client.operation.time_per_output_chunk",
			metric.WithUnit("s"),
			metric.WithDescription("Time between consecutive output chunks of a streaming GenAI response."),
			metric.WithExplicitBucketBoundaries(metricBucketsDuration...),
		)
		i.clientTokenUsage, _ = meter.Int64Histogram(
			"gen_ai.client.token.usage",
			metric.WithUnit("{token}"),
			metric.WithDescription("Number of tokens used in a GenAI client request, broken down by token type."),
			metric.WithExplicitBucketBoundaries(metricBucketsTokenUsage...),
		)

		inst = i
	})
	return inst
}
