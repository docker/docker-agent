package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

func TestNewOTelResourceUsesCurrentSchemaURL(t *testing.T) {
	t.Parallel()

	res, err := newOTelResource()
	require.NoError(t, err)
	assert.Equal(t, semconv.SchemaURL, res.SchemaURL())
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
