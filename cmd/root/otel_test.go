package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

func TestNewOTelResourceUsesCurrentSchemaURL(t *testing.T) {
	t.Parallel()

	res, err := newOTelResource()
	require.NoError(t, err)
	assert.Equal(t, semconv.SchemaURL, res.SchemaURL())
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
