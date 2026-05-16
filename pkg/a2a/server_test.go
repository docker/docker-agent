package a2a

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoutableAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ipv6 wildcard", "[::]:8080", "localhost:8080"},
		{"ipv4 wildcard", "0.0.0.0:8080", "localhost:8080"},
		{"empty host", ":8080", "localhost:8080"},
		{"localhost stays", "localhost:8080", "localhost:8080"},
		{"ipv4 loopback stays", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"specific ip stays", "192.168.1.1:9090", "192.168.1.1:9090"},
		{"hostname stays", "my-host:8080", "my-host:8080"},
		{"invalid addr returned as-is", "not-a-host-port", "not-a-host-port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, routableAddr(tt.addr))
		})
	}
}
