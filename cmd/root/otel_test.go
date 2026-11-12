package root

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// A literal, not semconv.SchemaURL, so a semconv bump is an explicit,
// reviewed change. NoError also guards against ErrSchemaURLConflict if
// resource.Default()'s semconv version diverges.
func TestNewOTelResourceUsesCurrentSchemaURL(t *testing.T) {
	t.Parallel()

	res, err := newOTelResource()
	require.NoError(t, err)
	assert.Equal(t, "https://opentelemetry.io/schemas/1.43.0", res.SchemaURL())
}

// TestTraceExportEmitsResourceSchemaURL verifies the schema URL on the
// wire: the OTLP payload's resource schema_url must be the documented
// literal, while the scope schema_url stays empty because docker-agent
// sets none on its own tracers.
func TestTraceExportEmitsResourceSchemaURL(t *testing.T) {
	// Not parallel: t.Setenv. Clear env that would alter sampling or
	// body encoding; endpoint env vars are already overridden by the
	// explicit WithEndpointURL.
	for _, key := range []string{
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
		"OTEL_EXPORTER_OTLP_COMPRESSION",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	// Empty ExportTraceServiceResponse: the full-success OTLP reply.
	respBody, err := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	require.NoError(t, err)

	type export struct {
		path    string
		body    []byte
		readErr error
	}
	exports := make(chan export, 4)

	// No assertions here — the handler runs on a server goroutine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		select {
		case exports <- export{path: r.URL.Path, body: body, readErr: readErr}:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(srv.Close)

	res, err := newOTelResource()
	require.NoError(t, err)
	tp, err := newTracerProvider(t.Context(), res, srv.URL)
	require.NoError(t, err)
	t.Cleanup(func() {
		// t.Context() is already canceled in cleanup; WithoutCancel
		// detaches it so shutdown still gets a bounded 5s window.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})

	_, span := tp.Tracer("test").Start(t.Context(), "schema-url-probe")
	span.End()
	require.NoError(t, tp.ForceFlush(t.Context()))

	var got export
	select {
	case got = <-exports:
	case <-time.After(10 * time.Second):
		t.Fatal("no OTLP trace export received")
	}
	require.NoError(t, got.readErr)
	assert.Equal(t, "/v1/traces", got.path)

	var req coltracepb.ExportTraceServiceRequest
	require.NoError(t, proto.Unmarshal(got.body, &req))
	require.Len(t, req.ResourceSpans, 1)
	rs := req.ResourceSpans[0]
	assert.Equal(t, "https://opentelemetry.io/schemas/1.43.0", rs.SchemaUrl)
	require.Len(t, rs.ScopeSpans, 1)
	assert.Empty(t, rs.ScopeSpans[0].SchemaUrl)
}

// TestProvidersWithoutEndpoint verifies all three providers build cleanly
// when no OTLP endpoint is configured — they're no-op exporters but must
// still produce valid, non-nil providers so callers can create instruments.
func TestProvidersWithoutEndpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	res, err := newOTelResource()
	require.NoError(t, err)

	tp, err := newTracerProvider(ctx, res, "")
	require.NoError(t, err)
	require.NotNil(t, tp)
	assert.NotNil(t, tp.Tracer("test"))

	mp, err := newMeterProvider(ctx, res, "")
	require.NoError(t, err)
	require.NotNil(t, mp)
	assert.NotNil(t, mp.Meter("test"))

	lp, err := newLoggerProvider(ctx, res, "")
	require.NoError(t, err)
	require.NotNil(t, lp)
	assert.NotNil(t, lp.Logger("test"))
}

// TestNormalizeOTLPEndpoint pins the bare-endpoint -> URL mapping the
// three OTLP/HTTP exporters share. Without this normalization the log
// exporter (insecure-by-default for bare hosts) conflicted with
// OTEL_EXPORTER_OTLP_CERTIFICATE and tore down the whole telemetry
// pipeline; the trace exporter (TLS-by-default for bare hosts) hid
// the inconsistency.
func TestNormalizeOTLPEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"bare remote host:port -> https", "alloy.observability.svc.cluster.local:4318", "https://alloy.observability.svc.cluster.local:4318"},
		{"bare remote host -> https", "example.com", "https://example.com"},
		{"bare localhost host:port -> http", "localhost:4318", "http://localhost:4318"},
		{"bare localhost -> http", "localhost", "http://localhost"},
		{"bare ipv4 loopback -> http", "127.0.0.1:4318", "http://127.0.0.1:4318"},
		{"bare ipv6 loopback -> http", "[::1]:4318", "http://[::1]:4318"},
		{"explicit https preserved", "https://example.com:4318", "https://example.com:4318"},
		{"explicit http preserved", "http://localhost:4318", "http://localhost:4318"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, normalizeOTLPEndpoint(tt.endpoint))
		})
	}
}

// TestSignalEndpointURL pins the base-endpoint -> per-signal URL mapping.
// docker-agent re-injects the configured endpoint through WithEndpointURL,
// which takes the path verbatim; signalEndpointURL restores the signal
// subpath the OTel SDK would otherwise append, so base-path backends such
// as Langfuse and LangSmith receive traces at <base>/v1/traces instead of
// 404ing on the bare base URL.
func TestSignalEndpointURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		endpoint   string
		signalPath string
		want       string
	}{
		{"langfuse base appends traces", "https://cloud.langfuse.com/api/public/otel", "/v1/traces", "https://cloud.langfuse.com/api/public/otel/v1/traces"},
		{"langsmith base appends traces", "https://api.smith.langchain.com/otel", "/v1/traces", "https://api.smith.langchain.com/otel/v1/traces"},
		{"full per-signal url preserved", "https://api.smith.langchain.com/otel/v1/traces", "/v1/traces", "https://api.smith.langchain.com/otel/v1/traces"},
		{"trailing slash base joins single slash", "https://cloud.langfuse.com/api/public/otel/", "/v1/logs", "https://cloud.langfuse.com/api/public/otel/v1/logs"},
		{"bare localhost host:port -> http + traces", "localhost:4318", "/v1/traces", "http://localhost:4318/v1/traces"},
		{"bare remote host:port -> https + metrics", "collector.example.com:4318", "/v1/metrics", "https://collector.example.com:4318/v1/metrics"},
		{"root-only endpoint appends traces", "https://collector.example.com", "/v1/traces", "https://collector.example.com/v1/traces"},
		{"explicit http preserved + logs", "http://localhost:4318", "/v1/logs", "http://localhost:4318/v1/logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, signalEndpointURL(tt.endpoint, tt.signalPath))
		})
	}
}

func TestIsLocalhostEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"localhost no port", "localhost", true},
		{"localhost with port", "localhost:4318", true},
		{"ipv4 loopback no port", "127.0.0.1", true},
		{"ipv4 loopback with port", "127.0.0.1:4318", true},
		{"ipv6 loopback no port", "::1", true},
		{"ipv6 loopback bracketed", "[::1]", true},
		{"ipv6 loopback with port", "[::1]:4318", true},
		{"remote host", "example.com", false},
		{"remote host with port", "example.com:4318", false},
		{"remote ip", "192.168.1.1", false},
		{"remote ip with port", "192.168.1.1:4318", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isLocalhostEndpoint(tt.endpoint))
		})
	}
}
