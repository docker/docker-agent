package dmrmodels

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
)

func TestResolveBaseURLCatalogDefault(t *testing.T) {
	t.Setenv("MODEL_RUNNER_HOST", "http://runner.example:12434")

	cfg := &latest.ModelConfig{Provider: "dmr", BaseURL: catalog.Lookup("dmr").BaseURL}
	endpoint, client := ResolveBaseURL(t.Context(), cfg, "")
	assert.Equal(t, "http://runner.example:12434/engines/v1/", endpoint)
	assert.Nil(t, client)
	assert.Equal(t, catalog.Lookup("dmr").BaseURL, cfg.BaseURL)

	cfg.BaseURL = "http://custom.example/v1"
	endpoint, client = ResolveBaseURL(t.Context(), cfg, "")
	assert.Equal(t, cfg.BaseURL, endpoint)
	assert.Nil(t, client)
}

func TestGetDMRFallbackURLs(t *testing.T) {
	t.Parallel()

	t.Run("inside container", func(t *testing.T) {
		t.Parallel()

		urls := getDMRFallbackURLs(true)

		// Should return 3 container-specific fallback URLs
		require.Len(t, urls, 3)

		// Verify the expected URLs in order (container-specific endpoints)
		assert.Equal(t, "http://model-runner.docker.internal/engines/v1/", urls[0])
		assert.Equal(t, "http://host.docker.internal:12434/engines/v1/", urls[1])
		assert.Equal(t, "http://172.17.0.1:12434/engines/v1/", urls[2])
	})

	t.Run("on host", func(t *testing.T) {
		t.Parallel()

		urls := getDMRFallbackURLs(false)

		// Should return 1 host-specific fallback URL
		require.Len(t, urls, 1)

		// Verify localhost is the only fallback on host
		assert.Equal(t, "http://127.0.0.1:12434/engines/v1/", urls[0])
	})
}

func TestDMRConnectivity(t *testing.T) {
	t.Parallel()

	t.Run("reachable endpoint", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/models", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer server.Close()

		result := testDMRConnectivity(t.Context(), server.Client(), server.URL+"/")
		assert.True(t, result)
	})

	t.Run("reachable endpoint with error response", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		// Should still return true because server is reachable
		result := testDMRConnectivity(t.Context(), server.Client(), server.URL+"/")
		assert.True(t, result)
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		t.Parallel()

		client := failingDialClient(t, errors.New("model endpoint unreachable"))
		result := testDMRConnectivity(t.Context(), client, "http://model-runner.invalid/")
		assert.False(t, result)
	})
}

func TestResolvedDockerTransportRetainsConnection(t *testing.T) {
	if inContainer() {
		t.Skip("Desktop engine routing is host-only")
	}
	t.Setenv("MODEL_RUNNER_HOST", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/exp/vDD4.40/engines/v1/models", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[{"id":"ai/test"}]}`))
	}))
	defer server.Close()
	ctx := ContextWithDockerConnection(t.Context(), nil, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	baseURL, client := ResolveBaseURL(ctx, nil, defaultContainerURL())
	require.NotNil(t, client)
	defer client.CloseIdleConnections()
	models, err := ListModelsAt(t.Context(), client, baseURL)
	require.NoError(t, err)
	assert.Equal(t, []string{"ai/test"}, models)
}

func TestSelectedDockerTransportErrorsNameEngine(t *testing.T) {
	if inContainer() {
		t.Skip("Desktop engine routing is host-only")
	}
	t.Setenv("MODEL_RUNNER_HOST", "")
	for _, tt := range []struct {
		name string
		err  error
		dial bool
	}{
		{name: "dial", err: net.ErrClosed, dial: true},
		{name: "closed pipe", err: os.ErrClosed},
		{name: "broken pipe", err: syscall.EPIPE},
		{name: "EOF", err: io.EOF, dial: true},
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded, dial: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ContextWithDockerConnection(t.Context(), []string{"--host=unix:///missing-dmr-engine.sock"}, func(context.Context) (net.Conn, error) {
				if tt.dial {
					return nil, tt.err
				}
				conn, peer := net.Pipe()
				t.Cleanup(func() { _ = peer.Close() })
				return &failingDockerConn{Conn: conn, err: tt.err}, nil
			})
			baseURL, client := ResolveBaseURL(ctx, nil, defaultContainerURL())
			require.NotNil(t, client)
			defer client.CloseIdleConnections()
			_, err := ListModelsAt(t.Context(), client, baseURL)
			require.ErrorIs(t, err, tt.err)
			require.ErrorContains(t, err, "unix:///missing-dmr-engine.sock")
			var requestErr *url.Error
			require.ErrorAs(t, err, &requestErr)
			assert.Equal(t, errors.Is(tt.err, context.DeadlineExceeded), requestErr.Timeout())
		})
	}
}

type failingDockerConn struct {
	net.Conn

	err error
}

func (c *failingDockerConn) Read([]byte) (int, error) {
	return 0, c.err
}

func (c *failingDockerConn) Write([]byte) (int, error) {
	return 0, c.err
}

func TestResolveSelectedDockerDoesNotProbeFallbacks(t *testing.T) {
	if inContainer() {
		t.Skip("Desktop engine routing is host-only")
	}
	t.Setenv("MODEL_RUNNER_HOST", "")
	calls := 0
	ctx := ContextWithDockerConnection(t.Context(), nil, func(context.Context) (net.Conn, error) {
		calls++
		return nil, errors.New("selected engine unavailable")
	})
	baseURL, client := ResolveBaseURL(ctx, nil, defaultContainerURL())
	require.NotNil(t, client)
	defer client.CloseIdleConnections()
	assert.Zero(t, calls, "selection must not probe or switch to a local runner")
	_, err := ListModelsAt(t.Context(), client, baseURL)
	require.ErrorContains(t, err, "selected engine unavailable")
	assert.Equal(t, 1, calls)
}
