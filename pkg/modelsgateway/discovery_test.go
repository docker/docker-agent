package modelsgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
)

// httptest servers use loopback URLs, so they exercise the no-Docker-token
// path unless a test supplies a Docker hostname with a rewriting transport.

func tokenEnv() environment.Provider {
	return environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "test-token",
	})
}

func TestListModels(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"openai/gpt-4o"},{"id":"anthropic/claude-sonnet-4-0"},{"id":""}]}`))
	}))
	defer server.Close()

	ids, err := ListModels(t.Context(), server.URL, tokenEnv())

	require.NoError(t, err)
	assert.Equal(t, []string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"}, ids)
	assert.Equal(t, "/v1/models", gotPath)
	assert.Equal(t, "Bearer test-token", gotAuth)
}

func TestListModels_GatewayPathAndQuery(t *testing.T) {
	t.Parallel()

	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer server.Close()

	ids, err := ListModels(t.Context(), server.URL+"/gateway/?api-version=2", tokenEnv())

	require.NoError(t, err)
	assert.Equal(t, []string{"gpt-4o"}, ids)
	assert.Equal(t, "/gateway/v1/models", gotPath)
	assert.Equal(t, "api-version=2", gotQuery)
}

func TestListModels_EmptyList(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer server.Close()

	ids, err := ListModels(t.Context(), server.URL, tokenEnv())

	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestListModels_Unsupported(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := ListModels(t.Context(), server.URL, tokenEnv())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestListModels_InvalidJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	_, err := ListModels(t.Context(), server.URL, tokenEnv())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding gateway models response")
}

func TestListModels_MissingDockerToken(t *testing.T) {
	t.Parallel()

	var gotAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer server.Close()

	client := server.Client()
	client.Transport = hostRewriteTransport{host: server.Listener.Addr().String(), base: client.Transport}
	_, err := listModelsWith(t.Context(), "https://models.docker.com", environment.NewMapEnvProvider(nil), client)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Docker Desktop")
	assert.Empty(t, gotAuth)
}

func TestListModels_UnreachableGateway(t *testing.T) {
	t.Parallel()

	_, err := ListModels(t.Context(), "http://127.0.0.1:1", tokenEnv())

	require.Error(t, err)
}

func TestListModels_InvalidURL(t *testing.T) {
	t.Parallel()

	_, err := ListModels(t.Context(), "http://[::1", tokenEnv())

	require.Error(t, err)
}

// hostRewriteTransport redirects every request to a local test server,
// letting tests exercise non-trusted hostnames without touching the network.
type hostRewriteTransport struct {
	host   string
	base   http.RoundTripper
	scheme string
}

func (t hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	if t.scheme != "" {
		r2.URL.Scheme = t.scheme
	}
	r2.URL.Host = t.host
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r2)
}

func TestListModels_GenericGatewayNeedsNoDockerToken(t *testing.T) {
	t.Parallel()

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer server.Close()

	// models.example.com is not a trusted Docker URL, so discovery must
	// neither require the Docker token nor send any Authorization header.
	client := &http.Client{Transport: hostRewriteTransport{host: server.Listener.Addr().String(), scheme: "http"}}
	ids, err := listModelsWith(t.Context(), "https://models.example.com", environment.NewMapEnvProvider(nil), client)

	require.NoError(t, err)
	assert.Equal(t, []string{"gpt-4o"}, ids)
	assert.Empty(t, gotAuth, "a generic gateway must not receive the Docker token")
}

func TestListModels_CallerContextDeadlineWins(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Hold the response until the client gives up, so the test measures
		// which deadline fires without leaving a stuck handler behind.
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ListModels(ctx, server.URL, tokenEnv())

	require.Error(t, err)
	assert.Less(t, time.Since(start), listTimeout, "a caller deadline shorter than the internal timeout must win")
}
