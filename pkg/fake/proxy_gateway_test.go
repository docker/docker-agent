package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	evaluatorprovider "github.com/docker/docker-agent/pkg/evaluator/provider"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/options"
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
			transport := hostRewriteRoundTripper{target: upstream.URL, base: upstream.Client().Transport}
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
	base   http.RoundTripper
}

func (t hostRewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := http.NewRequestWithContext(req.Context(), req.Method, t.target+req.URL.RequestURI(), req.Body)
	if err != nil {
		return nil, err
	}
	target.Header = req.Header.Clone()
	return t.base.RoundTrip(target)
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

func TestTypeSafeRecordingAndReplay(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "typesafe-key")
	const response = `{"model":"jev-1.13.0","answers":{"evaluation":{"type":"noul","noul":1}},"usage":{"input_tokens":12,"output_tokens":0}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/systemone", r.URL.Path)
		assert.Equal(t, "Bearer typesafe-key", r.Header.Get("Authorization"))
		_, err := io.WriteString(w, response)
		assert.NoError(t, err)
	}))
	t.Cleanup(upstream.Close)
	path := t.TempDir() + "/typesafe"
	proxyURL, cleanup, err := startStreamingRecordingProxy(t.Context(), path, "", APIKeyHeaderUpdater, hostRewriteRoundTripper{target: upstream.URL, base: upstream.Client().Transport})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cleanup()) })
	request := func(proxyURL string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxyURL+"/v1/systemone", strings.NewReader(`{"model":"jev-latest","state":"Docker?","questions":{"evaluation":{"type":"noul","instructions":"Is this Docker?"}}}`))
		require.NoError(t, err)
		req.Header.Set("X-Cagent-Forward", "https://api.typesafe.ai")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.JSONEq(t, response, string(body))
	}
	request(proxyURL)
	require.NoError(t, cleanup())
	data, err := os.ReadFile(path + ".yaml")
	require.NoError(t, err)
	assert.NotContains(t, string(data), "typesafe-key")
	proxyURL, replayCleanup, err := StartProxy(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, replayCleanup()) })
	request(proxyURL)
}

func TestEvaluatorRecordingGatewayDoesNotLeakCredentials(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "typesafe-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	for _, path := range []string{"/v1/systemone", "/v1/decisions", "/gateway/v1/systemone", "/gateway/v1/decisions", "/development/predict"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://gateway.example.com"+path, http.NoBody)
		req.Header.Set("X-Cagent-Evaluator", "1")
		req.Header.Set("Authorization", "Bearer docker-secret")
		req.Header.Set("X-Api-Key", "docker-secret")
		host := "https://api.typesafe.ai"
		if strings.HasSuffix(path, "/v1/decisions") {
			host = "https://api.openai.com/v1"
		}
		gatewayAuthHeaderUpdater("https://gateway.example.com")(host, req)
		assert.Empty(t, req.Header.Get("Authorization"))
		assert.Empty(t, req.Header.Get("X-Api-Key"))
	}
}

func TestEvaluatorRecordingPreservesEnvironmentProviderKey(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer resolved-key", r.Header.Get("Authorization"))
				response := `{"model":"jev","answers":{"evaluation":{"type":"noul","noul":1}}}`
				if backend == "openai" {
					assert.Equal(t, "/v1/decisions", r.URL.Path)
					response = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"evaluation","probability":1}]}`
				} else {
					assert.Equal(t, "/v1/systemone", r.URL.Path)
				}
				_, err := io.WriteString(w, response)
				assert.NoError(t, err)
			}))
			t.Cleanup(upstream.Close)
			proxyURL, cleanup, err := startStreamingRecordingProxy(t.Context(), t.TempDir()+"/evaluator", "", APIKeyHeaderUpdater, hostRewriteRoundTripper{target: upstream.URL, base: upstream.Client().Transport})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, cleanup()) })
			client, err := evaluatorprovider.New(t.Context(), latest.EvaluatorConfig{
				Provider: backend, Model: "model", Type: "boolean", Instructions: "Assess.", TokenKey: "CUSTOM_KEY_NOT_IN_OS",
			}, environment.NewMapEnvProvider(map[string]string{"CUSTOM_KEY_NOT_IN_OS": "resolved-key"}),
				options.WithHTTPTransportWrapper(RecordingTransport(proxyURL)))
			require.NoError(t, err)
			_, err = client.Evaluate(t.Context(), "state")
			require.NoError(t, err)
		})
	}
}

func TestEvaluatorRecordingPrivateOriginStaysDirect(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/development/predict", r.URL.Path)
		assert.Equal(t, "Bearer private-key", r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("X-Cagent-Forward"))
		_, err := io.WriteString(w, `{"model":"english","answers":{"evaluation":{"type":"noul","noul":1}}}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(upstream.Close)
	client, err := evaluatorprovider.New(t.Context(), latest.EvaluatorConfig{
		Provider: "typesafe", Model: "english", Type: "boolean", Instructions: "Assess.",
		Endpoint: upstream.URL + "/development/predict", TokenKey: "PRIVATE_KEY",
	}, environment.NewMapEnvProvider(map[string]string{"PRIVATE_KEY": "private-key"}),
		options.WithHTTPTransportWrapper(RecordingTransport("http://127.0.0.1:1")))
	require.NoError(t, err)
	_, err = client.Evaluate(t.Context(), "state")
	require.NoError(t, err)
}

func TestEvaluatorRecordingNeverFollowsRedirects(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			for _, throughGateway := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/gateway=%t", backend, status, throughGateway), func(t *testing.T) {
					t.Parallel()
					destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						t.Error("recorded evaluator followed a redirect with private evidence")
					}))
					t.Cleanup(destination.Close)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Location", destination.URL+"/stolen")
						w.WriteHeader(status)
					}))
					t.Cleanup(upstream.Close)
					gateway := ""
					if throughGateway {
						gateway = upstream.URL
					}
					proxyURL, cleanup, err := startStreamingRecordingProxy(t.Context(), t.TempDir()+"/redirect", gateway, nil, hostRewriteRoundTripper{target: upstream.URL, base: upstream.Client().Transport})
					require.NoError(t, err)
					t.Cleanup(func() { assert.NoError(t, cleanup()) })
					cfg := latest.EvaluatorConfig{Provider: backend, Model: "model", Type: "boolean", Instructions: "Assess."}
					client, err := evaluatorprovider.New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{environment.DockerDesktopTokenEnv: "docker-secret"}),
						options.WithGateway(proxyURL), options.WithEncryptedConfig("PRIVATE-CONFIG"))
					require.NoError(t, err)
					_, err = client.Evaluate(t.Context(), "PRIVATE-EVIDENCE")
					require.ErrorContains(t, err, fmt.Sprintf("HTTP status %d", status))
				})
			}
		}
	}
}

func TestCustomEvaluatorGatewayRecordingReplay(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			body := `{"model":"model","answers":{"evaluation":{"type":"noul","noul":1}}}`
			if backend == "openai" {
				body = `{"model":"model","answers":[{"type":"predicate","name":"evaluation","probability":1}]}`
			}
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/development%2Fpredict", r.URL.EscapedPath())
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			t.Cleanup(gateway.Close)
			path := t.TempDir() + "/custom"
			proxyURL, cleanup, err := startStreamingRecordingProxy(t.Context(), path, gateway.URL, gatewayAuthHeaderUpdater(gateway.URL), gateway.Client().Transport)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, cleanup()) })
			cfg := latest.EvaluatorConfig{Provider: backend, Model: "model", Type: "boolean", Instructions: "Assess.", Endpoint: "https://private.example.com/development%2Fpredict"}
			client, err := evaluatorprovider.New(t.Context(), cfg, environment.NewNoEnvProvider(), options.WithGateway(proxyURL), options.WithEncryptedConfig("opaque-encrypted-config"))
			require.NoError(t, err)
			_, err = client.Evaluate(t.Context(), json.RawMessage(`{"integer":9007199254740993}`))
			require.NoError(t, err)
			require.NoError(t, cleanup())
			proxyURL, replayCleanup, err := StartProxy(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, replayCleanup()) })
			cfg.BypassModelsGateway = true
			client, err = evaluatorprovider.New(t.Context(), cfg, environment.NewNoEnvProvider(),
				options.WithTokenSource(func(context.Context) (string, error) { return "", nil }), options.WithHTTPTransportWrapper(ReplayTransport(proxyURL)))
			require.NoError(t, err)
			_, err = client.Evaluate(t.Context(), json.RawMessage(`{"integer":9007199254740993}`))
			require.NoError(t, err)
			_, err = client.Evaluate(t.Context(), json.RawMessage(`{"integer":9007199254740992}`))
			require.Error(t, err, "an unmatched cassette must never call the upstream")
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}
