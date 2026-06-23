package root

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/version"
)

const AppName = "docker-agent"

// initOTelSDK initializes OpenTelemetry SDK with OTLP exporter
func initOTelSDK(ctx context.Context) (err error) {
	res, err := newOTelResource()
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	tp, err := newTracerProvider(ctx, res, endpoint)
	if err != nil {
		return fmt.Errorf("failed to create tracer provider: %w", err)
	}
	otel.SetTracerProvider(tp)

	mp, err := newMeterProvider(ctx, res, endpoint)
	if err != nil {
		_ = shutdownTracerProvider(tp)
		return fmt.Errorf("failed to create meter provider: %w", err)
	}
	otel.SetMeterProvider(mp)

	lp, err := newLoggerProvider(ctx, res, endpoint)
	if err != nil {
		_ = mp.Shutdown(context.Background())
		_ = shutdownTracerProvider(tp)
		return fmt.Errorf("failed to create logger provider: %w", err)
	}
	global.SetLoggerProvider(lp)

	// Set the global text-map propagator unconditionally so otelhttp
	// (and any other propagation-aware instrumentation) injects W3C
	// `traceparent` / `tracestate` / `baggage` on outbound requests
	// and extracts them on incoming ones. The propagator is a global
	// no-op until set; without this the SDK records spans locally
	// but they never chain across processes — `gen_ai.conversation.id`
	// baggage and the MCP `_meta` / sandbox env-var injectors are
	// dormant until this runs.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Single source of truth for "is OTel enabled?" — flip the
	// httpclient gate now so outbound requests start emitting CLIENT
	// spans and injecting traceparent. Previously the gate read
	// OTEL_EXPORTER_OTLP_ENDPOINT directly, which diverged from the
	// `--otel` CLI gate that controls this function: we'd either
	// initialise providers without HTTP wrapping, or wrap HTTP without
	// having a usable propagator.
	httpclient.SetOTelEnabled(true)

	go func() {
		<-ctx.Done()
		// Flush in dependency order: logs and metrics first (they may
		// reference active spans), then traces. Each provider gets its
		// own 5s budget so a slow exporter can't starve the others —
		// sharing a single timeout meant a stuck logs endpoint silently
		// dropped buffered metrics and spans.
		shutdown := func(fn func(context.Context) error) {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = fn(c)
		}
		shutdown(lp.Shutdown)
		shutdown(mp.Shutdown)
		shutdown(tp.Shutdown)
	}()

	return nil
}

// newTracerProvider builds the SDK tracer provider with an OTLP/HTTP
// exporter when an endpoint is set.
func newTracerProvider(ctx context.Context, res *resource.Resource, endpoint string) (*trace.TracerProvider, error) {
	opts := []trace.TracerProviderOption{trace.WithResource(res)}

	if endpoint == "" {
		return trace.NewTracerProvider(opts...), nil
	}

	exp, err := otlptracehttp.New(ctx, traceExporterOptions(endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}
	opts = append(opts, trace.WithBatcher(exp,
		trace.WithBatchTimeout(5*time.Second),
		trace.WithMaxExportBatchSize(512),
	))
	return trace.NewTracerProvider(opts...), nil
}

// newMeterProvider builds the SDK meter provider. Without an endpoint the
// provider still wires up so meters callers create are valid no-ops; with
// an endpoint, a periodic reader exports via OTLP/HTTP.
func newMeterProvider(ctx context.Context, res *resource.Resource, endpoint string) (*metric.MeterProvider, error) {
	opts := []metric.Option{metric.WithResource(res)}

	if endpoint != "" {
		exp, err := otlpmetrichttp.New(ctx, metricExporterOptions(endpoint)...)
		if err != nil {
			return nil, fmt.Errorf("failed to create metric exporter: %w", err)
		}
		opts = append(opts, metric.WithReader(metric.NewPeriodicReader(exp,
			metric.WithInterval(60*time.Second),
		)))
	}

	return metric.NewMeterProvider(opts...), nil
}

// newLoggerProvider builds the SDK logger provider. Required for the
// gen_ai.client.operation.exception event (a log record per spec) and for
// any future log-bridge instrumentation.
func newLoggerProvider(ctx context.Context, res *resource.Resource, endpoint string) (*log.LoggerProvider, error) {
	opts := []log.LoggerProviderOption{log.WithResource(res)}

	if endpoint != "" {
		exp, err := otlploghttp.New(ctx, logExporterOptions(endpoint)...)
		if err != nil {
			return nil, fmt.Errorf("failed to create log exporter: %w", err)
		}
		opts = append(opts, log.WithProcessor(log.NewBatchProcessor(exp)))
	}

	return log.NewLoggerProvider(opts...), nil
}

// normalizeOTLPEndpoint turns a possibly-bare `host:port` into a fully
// scheme-qualified URL so all three OTLP/HTTP exporters can be wired via
// `WithEndpointURL` consistently. We can't rely on the SDKs' default
// scheme inference: `otlptracehttp` (older API) treats a bare endpoint
// as TLS-by-default while `otlploghttp` (newer API) treats the same
// bare endpoint as insecure-by-default. With `OTEL_EXPORTER_OTLP_CERTIFICATE`
// set in the env, the log exporter then errors out with
// `insecure HTTP endpoint cannot use TLS client configuration`,
// `initOTelSDK` propagates the failure, and the entire telemetry
// pipeline (including traces) is torn down.
//
// Pinning the scheme up front removes that asymmetry: localhost gets
// `http://`, every other host gets `https://`, and any explicit scheme
// the caller already supplied is honoured verbatim.
func normalizeOTLPEndpoint(endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	if isLocalhostEndpoint(endpoint) {
		return "http://" + endpoint
	}
	return "https://" + endpoint
}

func traceExporterOptions(endpoint string) []otlptracehttp.Option {
	return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(normalizeOTLPEndpoint(endpoint))}
}

func metricExporterOptions(endpoint string) []otlpmetrichttp.Option {
	return []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(normalizeOTLPEndpoint(endpoint))}
}

func logExporterOptions(endpoint string) []otlploghttp.Option {
	return []otlploghttp.Option{otlploghttp.WithEndpointURL(normalizeOTLPEndpoint(endpoint))}
}

func shutdownTracerProvider(tp *trace.TracerProvider) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tp.Shutdown(shutdownCtx)
}

func newOTelResource() (*resource.Resource, error) {
	// Standard OTel resource attributes; users can layer additional
	// labels via the spec-defined `OTEL_RESOURCE_ATTRIBUTES` env var,
	// which `resource.Default` merges in.
	attrs := []attribute.KeyValue{
		semconv.ServiceName(AppName),
		semconv.ServiceVersion(version.Version),
		semconv.ServiceInstanceID(uuid.NewString()),
		semconv.ProcessPID(os.Getpid()),
		semconv.ProcessRuntimeName("go"),
		semconv.OSTypeKey.String(runtime.GOOS),
		semconv.HostArchKey.String(runtime.GOARCH),
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		attrs = append(attrs, semconv.HostName(hostname))
	}
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
}

// isLocalhostEndpoint reports whether the given endpoint refers to a
// loopback address so that we can safely skip TLS.
func isLocalhostEndpoint(endpoint string) bool {
	host := endpoint
	// Strip port if present.
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	// Strip brackets from IPv6 addresses (e.g. "[::1]" without a port).
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
