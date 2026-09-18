package base

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

type fakeEnv map[string]string

func (f fakeEnv) Get(_ context.Context, name string) (string, bool) {
	v, ok := f[name]
	return v, ok
}

func TestVerifyDockerGatewayAuth(t *testing.T) {
	t.Parallel()

	t.Run("non-docker gateway needs no token", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, VerifyDockerGatewayAuth(t.Context(), fakeEnv{}, "https://gateway.example.com"))
	})

	t.Run("loopback gateway needs no token", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, VerifyDockerGatewayAuth(t.Context(), fakeEnv{}, "http://localhost:8080"))
	})

	t.Run("trusted docker gateway with token", func(t *testing.T) {
		t.Parallel()
		env := fakeEnv{environment.DockerDesktopTokenEnv: "jwt"}
		assert.NoError(t, VerifyDockerGatewayAuth(t.Context(), env, "https://api.docker.com/models"))
	})

	t.Run("trusted docker gateway without token", func(t *testing.T) {
		t.Parallel()
		err := VerifyDockerGatewayAuth(t.Context(), fakeEnv{}, "https://api.docker.com/models")
		assert.EqualError(t, err, "sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
	})
}

func TestGatewayAuthToken(t *testing.T) {
	t.Parallel()

	t.Run("non-docker gateway returns empty token", func(t *testing.T) {
		t.Parallel()
		token, err := GatewayAuthToken(t.Context(), fakeEnv{}, "https://gateway.example.com")
		require.NoError(t, err)
		assert.Empty(t, token)
	})

	t.Run("loopback gateway returns empty token", func(t *testing.T) {
		t.Parallel()
		token, err := GatewayAuthToken(t.Context(), fakeEnv{}, "http://127.0.0.1:8080")
		require.NoError(t, err)
		assert.Empty(t, token)
	})

	t.Run("trusted docker gateway returns fresh token", func(t *testing.T) {
		t.Parallel()
		env := fakeEnv{environment.DockerDesktopTokenEnv: "jwt"}
		token, err := GatewayAuthToken(t.Context(), env, "https://api.docker.com/models")
		require.NoError(t, err)
		assert.Equal(t, "jwt", token)
	})

	t.Run("trusted docker gateway without token", func(t *testing.T) {
		t.Parallel()
		_, err := GatewayAuthToken(t.Context(), fakeEnv{}, "https://api.docker.com/models")
		assert.EqualError(t, err, NoDesktopTokenErrorMessage)
	})
}

func TestGatewayHTTPOptions(t *testing.T) {
	t.Parallel()

	apply := func(opts []httpclient.Opt) *httpclient.HTTPOptions {
		o := &httpclient.HTTPOptions{Header: make(http.Header)}
		for _, opt := range opts {
			opt(o)
		}
		return o
	}

	gatewayURL, err := url.Parse("https://api.docker.com/models?tier=pro")
	require.NoError(t, err)

	t.Run("default base URL and identity headers", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", Name: "smart"}
		o := apply(GatewayHTTPOptions(gatewayURL, "https://api.openai.com/v1", cfg, &options.ModelOptions{}))
		assert.Equal(t, "https://api.openai.com/v1", o.Header.Get("X-Cagent-Forward"))
		assert.Equal(t, "openai", o.Header.Get("X-Cagent-Provider"))
		assert.Equal(t, "gpt-4o", o.Header.Get("X-Cagent-Model"))
		assert.Equal(t, "smart", o.Header.Get("X-Cagent-Model-Name"))
		assert.Equal(t, "pro", o.Query.Get("tier"))
		assert.Empty(t, o.Header.Get("X-Cagent-GeneratingTitle"))
		assert.Empty(t, o.Header.Get("X-Cagent-Compacting"))
	})

	t.Run("model base_url overrides the default", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: "https://example.com/v1"}
		o := apply(GatewayHTTPOptions(gatewayURL, "https://api.openai.com/v1", cfg, &options.ModelOptions{}))
		assert.Equal(t, "https://example.com/v1", o.Header.Get("X-Cagent-Forward"))
	})

	t.Run("title generation marker", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}
		modelOpts := options.Apply(options.WithGeneratingTitle())
		o := apply(GatewayHTTPOptions(gatewayURL, "https://api.openai.com/v1", cfg, &modelOpts))
		assert.Equal(t, "1", o.Header.Get("X-Cagent-GeneratingTitle"))
		assert.Empty(t, o.Header.Get("X-Cagent-Compacting"))
	})

	t.Run("compaction marker", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}
		modelOpts := options.Apply(options.WithCompacting())
		o := apply(GatewayHTTPOptions(gatewayURL, "https://api.openai.com/v1", cfg, &modelOpts))
		assert.Equal(t, "1", o.Header.Get("X-Cagent-Compacting"))
		assert.Empty(t, o.Header.Get("X-Cagent-GeneratingTitle"))
	})

	t.Run("nil model options adds no markers", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}
		o := apply(GatewayHTTPOptions(gatewayURL, "https://api.openai.com/v1", cfg, nil))
		assert.Equal(t, "https://api.openai.com/v1", o.Header.Get("X-Cagent-Forward"))
		assert.Empty(t, o.Header.Get("X-Cagent-GeneratingTitle"))
		assert.Empty(t, o.Header.Get("X-Cagent-Compacting"))
	})

	t.Run("encrypted config forwarded to a trusted Docker gateway sets the body option, not a header", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}
		modelOpts := options.Apply(options.WithEncryptedConfig("ENC-BLOB"))
		o := apply(GatewayHTTPOptions(gatewayURL, "https://api.openai.com/v1", cfg, &modelOpts))
		assert.Empty(t, o.Header.Get("X-Cagent-Encrypted-Config"), "encrypted config must not travel as a header")
		assert.Equal(t, "ENC-BLOB", o.EncryptedConfigBody(), "encrypted config must be set as the body option")
	})

	t.Run("encrypted config is not forwarded to an untrusted gateway", func(t *testing.T) {
		t.Parallel()
		untrusted, err := url.Parse("https://gateway.example.com/v1")
		require.NoError(t, err)
		cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}
		modelOpts := options.Apply(options.WithEncryptedConfig("ENC-BLOB"))
		o := apply(GatewayHTTPOptions(untrusted, "https://api.openai.com/v1", cfg, &modelOpts))
		assert.Empty(t, o.Header.Get("X-Cagent-Encrypted-Config"))
		assert.Empty(t, o.EncryptedConfigBody(), "untrusted gateway must not receive the encrypted config")
	})
}

func TestNewGatewayClientBaseURL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		gateway string
		suffix  string
		want    string
	}{
		{"openai", "https://gateway.example.com/models?tier=pro#fragment", "/v1/", "https://gateway.example.com/models/v1/"},
		{"anthropic and gemini", "https://gateway.example.com/models", "/", "https://gateway.example.com/models/"},
		{"trailing slash", "https://gateway.example.com/models/", "/v1/", "https://gateway.example.com/models//v1/"},
		{"escaped path", "https://gateway.example.com/a%2Fb", "/", "https://gateway.example.com/a/b/"},
		{"relative URL", "models", "/", "://models/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			connection, err := NewGatewayClient(t.Context(), nil, tc.gateway, "https://api.example.com", tc.suffix, &latest.ModelConfig{}, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, connection.BaseURL)
			assert.Empty(t, connection.AuthToken)
			require.NotNil(t, connection.HTTPClient)
			assert.NotNil(t, connection.HTTPClient.Transport)
		})
	}
}

func TestNewGatewayClientExtraOptions(t *testing.T) {
	t.Parallel()

	modelOpts := options.Apply(options.WithHTTPTransportWrapper(func(rt http.RoundTripper) http.RoundTripper { return nil }))
	connection, err := NewGatewayClient(t.Context(), nil, "https://gateway.example.com", "https://api.example.com", "/", &latest.ModelConfig{Provider: "openai", BaseURL: "https://custom.example.com"}, &modelOpts,
		func(o *httpclient.HTTPOptions) {
			assert.Equal(t, "https://custom.example.com", o.Header.Get("X-Cagent-Forward"))
			assert.Equal(t, "openai", o.Header.Get("X-Cagent-Provider"))
		},
	)
	require.NoError(t, err)
	assert.NotNil(t, connection.HTTPClient.Transport, "a nil wrapper result must preserve the gateway transport")
}

func TestNewGatewayClientAuthRetry(t *testing.T) {
	t.Parallel()

	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		requests <- auth
		if auth == "Bearer token-1" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(server.Close)

	calls := 0
	env := gatewayTokenEnv(func() string {
		calls++
		return fmt.Sprintf("token-%d", calls)
	})
	wraps := 0
	modelOpts := options.Apply(options.WithHTTPTransportWrapper(func(rt http.RoundTripper) http.RoundTripper {
		wraps++
		return rt
	}))
	connection, err := NewGatewayClient(t.Context(), env, server.URL, "https://api.example.com", "/", &latest.ModelConfig{}, &modelOpts)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, connection.BaseURL, http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+connection.AuthToken)
	resp, err := connection.HTTPClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, requests, 2)
	assert.Equal(t, "Bearer token-1", <-requests)
	assert.Equal(t, "Bearer token-2", <-requests)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, wraps, "retry must reuse the wrapped transport")
}

type gatewayTokenEnv func() string

func (f gatewayTokenEnv) Get(_ context.Context, name string) (string, bool) {
	if name != environment.DockerDesktopTokenEnv {
		return "", false
	}
	return f(), true
}
