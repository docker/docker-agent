package mcp

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestCreateToolSetStandaloneSSE(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		runConfig := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
		toolset := latest.Toolset{
			Type:   "mcp",
			Remote: latest.Remote{URL: "https://mcp.example.com/mcp", TransportType: "streamable-http", StandaloneSSE: enabled},
		}

		ts, err := CreateToolSet(WithOAuthTokenStore(t.Context(), NewInMemoryTokenStore()), toolset, runConfig)
		require.NoError(t, err)

		client, ok := ts.(*Toolset).mcpClient.(*remoteMCPClient)
		require.True(t, ok)
		client.mu.RLock()
		got := client.standaloneSSE
		client.mu.RUnlock()
		assert.Equal(t, enabled, got)
	}
}

func TestRemoteClientStandaloneSSEOpensGetStream(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			t.Parallel()

			var gets atomic.Int32
			handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server {
				return gomcp.NewServer(&gomcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
			}, nil)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)

			client := newRemoteClient(srv.URL, "streamable-http", nil, NewInMemoryTokenStore(), nil, true, nil)
			client.SetStandaloneSSE(enabled)
			_, err := client.Initialize(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close(t.Context()) })

			if enabled {
				assert.Eventually(t, func() bool { return gets.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
				return
			}
			time.Sleep(200 * time.Millisecond) //nolint:forbidigo // Absence of a GET stream can only be asserted after a real-time grace period.
			assert.Zero(t, gets.Load())
		})
	}
}
