package fake

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"

	"github.com/docker/docker-agent/pkg/httpclient"
)

func TestStartRecordingProxy_EncryptedConfigSecrecy(t *testing.T) {
	const (
		encrypted = "ENCRYPTED-AGENT-CONFIG"
		digest    = "sha256:DIGEST-SECRET"
	)

	tests := []struct {
		name              string
		upstreamTrustURL  string
		wantUpstreamField bool
	}{
		{name: "untrusted upstream", upstreamTrustURL: "https://gateway.example.com"},
		{name: "trusted loopback upstream", upstreamTrustURL: "http://localhost:8080", wantUpstreamField: true},
		{name: "trusted Docker upstream", upstreamTrustURL: "https://models.docker.com", wantUpstreamField: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				upstreamBody, err = io.ReadAll(r.Body)
				assert.NoError(t, err)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()

			cassettePath := t.TempDir() + "/recording"
			transport := hostRewriteRoundTripper{target: upstream.URL}
			proxyURL, cleanup, err := startStreamingRecordingProxy(t.Context(), cassettePath, tt.upstreamTrustURL,
				gatewayAuthHeaderUpdater(tt.upstreamTrustURL), transport)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cleanup() })

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","encrypted_agent_config":"`+encrypted+`"}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cagent-Forward", "https://api.openai.com/v1")
			req.Header.Set(httpclient.EncryptedConfigDigestHeader, digest)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			require.NoError(t, cleanup())

			if tt.wantUpstreamField {
				assert.Contains(t, string(upstreamBody), encrypted)
			} else {
				assert.NotContains(t, string(upstreamBody), encrypted)
			}

			data, err := os.ReadFile(cassettePath + ".yaml")
			require.NoError(t, err)
			assert.NotContains(t, string(data), encrypted)
			assert.NotContains(t, string(data), digest)
		})
	}
}

type hostRewriteRoundTripper struct {
	target string
}

func (t hostRewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := http.NewRequestWithContext(req.Context(), req.Method, t.target+req.URL.RequestURI(), req.Body)
	if err != nil {
		return nil, err
	}
	target.Header = req.Header.Clone()
	return http.DefaultTransport.RoundTrip(target)
}

func TestStartRecordingProxy_NoUpstreamScrubsEncryptedConfig(t *testing.T) {
	const encrypted = "NO-UPSTREAM-SECRET"

	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		assert.NoError(t, err)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cassettePath := t.TempDir() + "/recording"
	proxyURL, cleanup, err := StartStreamingRecordingProxy(t.Context(), cassettePath, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup() })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","encrypted_agent_config":"`+encrypted+`"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cagent-Forward", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.NoError(t, cleanup())

	assert.NotContains(t, string(upstreamBody), encrypted)
	data, err := os.ReadFile(cassettePath + ".yaml")
	require.NoError(t, err)
	assert.NotContains(t, string(data), encrypted)
}

func TestGatewayTargetURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		gateway string
		path    string
		want    string
	}{
		{
			name:    "root gateway",
			gateway: "https://gateway.example.com",
			path:    "/v1/messages",
			want:    "https://gateway.example.com/v1/messages",
		},
		{
			name:    "gateway with trailing slash",
			gateway: "https://gateway.example.com/",
			path:    "/v1/messages",
			want:    "https://gateway.example.com/v1/messages",
		},
		{
			name:    "gateway with path prefix",
			gateway: "https://api.docker.com/models",
			path:    "/v1/chat/completions",
			want:    "https://api.docker.com/models/v1/chat/completions",
		},
		{
			name:    "merges gateway and request query",
			gateway: "https://api.docker.com/models?tier=pro",
			path:    "/v1/chat/completions?stream=true",
			want:    "https://api.docker.com/models/v1/chat/completions?stream=true&tier=pro",
		},
		{
			name:    "preserves percent-encoded path segments",
			gateway: "https://gateway.example.com",
			path:    "/v1/models/org%2Fmodel",
			want:    "https://gateway.example.com/v1/models/org%2Fmodel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, http.NoBody)
			got, err := GatewayTargetURL(tt.gateway, req)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGatewayAuthHeaderUpdater(t *testing.T) {
	newReq := func(t *testing.T) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://gateway.example.com/v1/messages", http.NoBody)
		require.NoError(t, err)
		return req
	}

	t.Run("docker gateway keeps the Desktop token the client attached", func(t *testing.T) {
		req := newReq(t)
		req.Header.Set("Authorization", "Bearer desktop-jwt")
		req.Header.Set("X-Api-Key", "desktop-jwt")

		gatewayAuthHeaderUpdater("https://api.docker.com/models")("https://api.anthropic.com", req)

		assert.Equal(t, "Bearer desktop-jwt", req.Header.Get("Authorization"))
		assert.Equal(t, "desktop-jwt", req.Header.Get("X-Api-Key"))
	})

	t.Run("non-docker gateway strips the Desktop token", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")

		req := newReq(t)
		req.Header.Set("Authorization", "Bearer desktop-jwt")
		req.Header.Set("X-Api-Key", "desktop-jwt")
		req.Header.Set("X-Goog-Api-Key", "desktop-jwt")

		gatewayAuthHeaderUpdater("https://gateway.example.com")("https://api.anthropic.com", req)

		assert.Empty(t, req.Header.Get("Authorization"), "Desktop token must not leak to non-Docker gateways")
		assert.Empty(t, req.Header.Get("X-Api-Key"))
		assert.Empty(t, req.Header.Get("X-Goog-Api-Key"))
	})

	t.Run("non-docker gateway re-applies the provider env key", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "env-key")

		req := newReq(t)
		req.Header.Set("Authorization", "Bearer desktop-jwt")
		req.Header.Set("X-Api-Key", "desktop-jwt")

		gatewayAuthHeaderUpdater("https://gateway.example.com")("https://api.anthropic.com", req)

		assert.Equal(t, "env-key", req.Header.Get("X-Api-Key"))
		assert.Empty(t, req.Header.Get("Authorization"))
	})
}

// Recording through an upstream gateway must forward the request to the
// gateway (with the auth the client would send to it directly), not the
// provider's public endpoint, and record the interaction under the canonical
// provider URL so the cassette stays replayable.
func TestStartRecordingProxy_UpstreamGateway(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "env-key")

	var gotPath, gotAPIKey, gotForward string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotForward = r.Header.Get("X-Cagent-Forward")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gateway.Close()

	cassettePath := t.TempDir() + "/recording"
	// gatewayAuthHeaderUpdater for a non-Docker gateway; passed explicitly
	// because the httptest gateway is localhost, which the Docker token
	// trust check would otherwise match.
	proxyURL, cleanup, err := StartStreamingRecordingProxy(t.Context(), cassettePath, gateway.URL,
		gatewayAuthHeaderUpdater("https://gateway.example.com"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup() }) // idempotent; defensive if asserts fail early

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/messages", strings.NewReader(`{"model":"claude"}`))
	require.NoError(t, err)
	req.Header.Set("X-Cagent-Forward", "https://api.anthropic.com")
	req.Header.Set("X-Api-Key", "desktop-jwt")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"ok":true}`, string(body))
	assert.Equal(t, "/v1/messages", gotPath)
	assert.Equal(t, "env-key", gotAPIKey, "gateway must receive the provider env key, not the Desktop token")
	assert.Equal(t, "https://api.anthropic.com", gotForward, "forward header must reach the upstream gateway")

	require.NoError(t, cleanup())

	data, err := os.ReadFile(cassettePath + ".yaml")
	require.NoError(t, err)
	c, err := cassette.Load(cassettePath)
	require.NoError(t, err)
	require.Len(t, c.Interactions, 1)
	assert.Equal(t, "https://api.anthropic.com/v1/messages", c.Interactions[0].Request.URL,
		"cassette must record the canonical provider URL, not the gateway URL")
	assert.NotContains(t, string(data), "env-key", "auth must not leak into the cassette")
	assert.NotContains(t, string(data), "desktop-jwt", "auth must not leak into the cassette")
}

// Unknown forward hosts (custom base_url models) recorded through a gateway
// must be saved under the forwarded host, never under the gateway URL whose
// query may carry secrets.
func TestStartRecordingProxy_UpstreamGateway_UnknownHost(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gateway.Close()

	cassettePath := t.TempDir() + "/recording"
	proxyURL, cleanup, err := StartStreamingRecordingProxy(t.Context(), cassettePath, gateway.URL+"?api_key=gateway-secret",
		gatewayAuthHeaderUpdater("https://gateway.example.com"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup() })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/chat/completions", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("X-Cagent-Forward", "https://custom.example.com/v1")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, cleanup())

	data, err := os.ReadFile(cassettePath + ".yaml")
	require.NoError(t, err)
	c, err := cassette.Load(cassettePath)
	require.NoError(t, err)
	require.Len(t, c.Interactions, 1)
	assert.Equal(t, "https://custom.example.com/v1/chat/completions", c.Interactions[0].Request.URL)
	assert.NotContains(t, string(data), "gateway-secret", "gateway query secrets must not leak into the cassette")
}

// A missing forward header in gateway mode is rejected rather than blindly
// forwarded and recorded.
func TestStartRecordingProxy_UpstreamGateway_MissingForwardHeader(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gateway.Close()

	cassettePath := t.TempDir() + "/recording"
	proxyURL, cleanup, err := StartStreamingRecordingProxy(t.Context(), cassettePath, gateway.URL,
		gatewayAuthHeaderUpdater("https://gateway.example.com"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup() })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/messages", http.NoBody)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestStartStreamingRecordingProxy_InvalidGatewayURL(t *testing.T) {
	t.Parallel()

	_, _, err := StartStreamingRecordingProxy(t.Context(), t.TempDir()+"/recording", "not a url", nil)
	require.ErrorContains(t, err, "invalid upstream gateway URL")
}

// Without an upstream gateway the proxy keeps its historical behavior:
// forward to the provider's public endpoint with env-provided API keys.
func TestStartRecordingProxy_NoGatewayRejectsUnknownHost(t *testing.T) {
	cassettePath := t.TempDir() + "/recording"
	proxyURL, cleanup, err := StartRecordingProxy(t.Context(), cassettePath, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/messages", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Cagent-Forward", "https://unknown.example.com")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
