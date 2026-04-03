package root

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/docker/cagent/pkg/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const AppName = "cagent"

// initOTelSDK initializes OpenTelemetry SDK with OTLP exporter
func initOTelSDK(ctx context.Context) (err error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(AppName),
			semconv.ServiceVersion(version.Version),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	var traceExporter trace.SpanExporter
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	// Only initialize if endpoint is configured
	if endpoint != "" {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
		if strings.HasPrefix(endpoint, "http://") {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		traceExporter, err = otlptracehttp.New(ctx, opts...)
		if err != nil {
			return fmt.Errorf("failed to create trace exporter: %w", err)
		}
	}

	// Configure tracer provider
	var tracerProviderOpts []trace.TracerProviderOption
	tracerProviderOpts = append(tracerProviderOpts, trace.WithResource(res))

	if traceExporter != nil {
		tracerProviderOpts = append(tracerProviderOpts,
			trace.WithBatcher(traceExporter,
				trace.WithBatchTimeout(5*time.Second),
				trace.WithMaxExportBatchSize(512),
			),
		)
	}

	tp := trace.NewTracerProvider(tracerProviderOpts...)
	otel.SetTracerProvider(tp)

	go func() {
		<-ctx.Done()
		_ = tp.Shutdown(context.Background())
	}()

	return nil
}
