package mcp

import (
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// metricBuckets matches the spec's bucket boundaries for all four MCP
// duration histograms (mcp.client/server.operation.duration and
// mcp.client/server.session.duration).
var metricBuckets = []float64{
	0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300,
}

type instruments struct {
	clientOperationDuration metric.Float64Histogram
	serverOperationDuration metric.Float64Histogram
	// mcp.{client,server}.session.duration histograms are defined by
	// the spec but require a SessionSpan that tracks open/close at
	// the transport layer. Wire those up alongside the transport
	// instrumentation; until then registering them here would create
	// always-empty time series in Mimir.
}

var (
	instOnce sync.Once
	inst     *instruments
)

func getInstruments() *instruments {
	instOnce.Do(func() {
		meter := otel.Meter(instrumentationName)
		i := &instruments{}

		// Histogram registration rarely fails; on the rare miss we
		// keep the successfully created instruments rather than
		// abandoning the whole package — record sites nil-check.
		i.clientOperationDuration, _ = meter.Float64Histogram(
			"mcp.client.operation.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Time taken by an MCP client to send a request and receive its response."),
			metric.WithExplicitBucketBoundaries(metricBuckets...),
		)
		i.serverOperationDuration, _ = meter.Float64Histogram(
			"mcp.server.operation.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Time taken by an MCP server to handle a request and send its response."),
			metric.WithExplicitBucketBoundaries(metricBuckets...),
		)

		inst = i
	})
	return inst
}
