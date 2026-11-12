package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestEvaluateGateway(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			path, forward, body, model := "/v1/systemone", defaultBaseURL, responseBody(`{"type":"noul","noul":0.9}`), "jev-test"
			if backend == "openai" {
				path, forward, model = "/v1/decisions", openAIBaseURL, "gpt-6-luna"
				body = openAIResponse(`{"type":"predicate","name":"evaluation","probability":0.9}`)
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/gateway"+path, r.URL.Path)
				assert.Equal(t, "paid", r.URL.Query().Get("tier"))
				assert.Equal(t, "Bearer "+r.Header.Get("X-Cagent-Session-Id"), r.Header.Get("Authorization"))
				assert.NotEmpty(t, r.Header.Get("X-Cagent-Id"))
				assert.Equal(t, forward, r.Header.Get("X-Cagent-Forward"))
				assert.Equal(t, backend, r.Header.Get("X-Cagent-Provider"))
				assert.Equal(t, model, r.Header.Get("X-Cagent-Model"))
				var payload map[string]json.RawMessage
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				assert.JSONEq(t, `"encrypted"`, string(payload[httpclient.EncryptedConfigBodyField]))
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			cfg := testConfig("boolean")
			cfg.Provider, cfg.Model = backend, model
			env := environmentFunc(func(ctx context.Context, name string) (string, bool) {
				assert.Equal(t, environment.DockerDesktopTokenEnv, name, "gateway calls must not request provider keys")
				return httpclient.SessionIDFromContext(ctx), true
			})
			client, err := New(t.Context(), cfg, env, options.WithGateway(server.URL+"/gateway/?tier=paid"), options.WithEncryptedConfig("encrypted"))
			require.NoError(t, err)
			assert.Zero(t, calls.Load())
			var wg sync.WaitGroup
			for _, session := range []string{"first", "second"} {
				wg.Go(func() {
					result, err := client.Evaluate(httpclient.ContextWithSessionID(t.Context(), session), map[string]string{"tool": "shell"})
					if assert.NoError(t, err) {
						assert.InDelta(t, 0.9, *result.Probability, 1e-9)
					}
				})
			}
			wg.Wait()
			assert.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestGatewayAuthRetry(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int64
			var firstBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				if requests.Add(1) == 1 {
					firstBody = string(body)
					assert.Equal(t, "Bearer stale", r.Header.Get("Authorization"))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				assert.Equal(t, firstBody, string(body))
				assert.Equal(t, "Bearer fresh", r.Header.Get("Authorization"))
				response := responseBody(`{"type":"noul","noul":1}`)
				if backend == "openai" {
					response = openAIResponse(`{"type":"predicate","name":"evaluation","probability":1}`)
				}
				_, err = io.WriteString(w, response)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			var lookups atomic.Int64
			env := environmentFunc(func(context.Context, string) (string, bool) {
				if lookups.Add(1) == 1 {
					return "stale", true
				}
				return "fresh", true
			})
			cfg := testConfig("boolean")
			cfg.Provider = backend
			client, err := New(t.Context(), cfg, env, options.WithGateway(server.URL), options.WithEncryptedConfig("encrypted"))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			_, err = client.Evaluate(ctx, "evidence")
			require.NoError(t, err)
			assert.EqualValues(t, 2, requests.Load())
			require.Len(t, records, 1, "the rejected authentication attempt is not a billable evaluation")
		})
	}
}

func TestGatewayBypass(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/predict", r.URL.Path)
				assert.Equal(t, "Bearer direct", r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("X-Cagent-Forward"))
				assert.Empty(t, r.Header.Get("X-Cagent-Session-Id"))
				body := responseBody(`{"type":"noul","noul":1}`)
				if backend == "openai" {
					body = openAIResponse(`{"type":"predicate","name":"evaluation","probability":1}`)
				}
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			cfg := testConfig("boolean")
			cfg.Provider, cfg.Endpoint, cfg.TokenKey, cfg.BypassModelsGateway = backend, server.URL+"/predict", "CUSTOM_KEY", true
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"CUSTOM_KEY": "direct"}), options.WithGateway("https://gateway.docker.com"))
			require.NoError(t, err)
			_, err = client.Evaluate(httpclient.ContextWithSessionID(t.Context(), "session"), "evidence")
			require.NoError(t, err)
		})
	}
}

func TestGatewayTrustAndValidation(t *testing.T) {
	t.Parallel()
	for _, gateway := range []string{"relative", "ftp://host", "https://", "http://%", "https://private-token@host", "https://host#private-token"} {
		client, err := New(t.Context(), testConfig("boolean"), environment.NewNoEnvProvider(), options.WithGateway(gateway))
		require.Error(t, err)
		assert.Nil(t, client)
		assert.NotContains(t, err.Error(), "private-token")
	}
	_, err := New(t.Context(), testConfig("boolean"), environment.NewNoEnvProvider(), options.WithGateway("https://gateway.docker.com"))
	require.ErrorContains(t, err, "sign in Docker Desktop")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NotContains(t, string(body), "encrypted")
		_, err = io.WriteString(w, responseBody(`{"type":"noul","noul":1}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	gateway := "https://gateway.example.com"
	client, err := New(t.Context(), testConfig("boolean"), environmentFunc(func(context.Context, string) (string, bool) {
		t.Error("untrusted gateway requested credentials")
		return "secret", true
	}), options.WithGateway(gateway), options.WithEncryptedConfig("encrypted"),
		options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
			return evaluatorRoundTripper(func(req *http.Request) (*http.Response, error) {
				req.URL.Scheme = "http"
				req.URL.Host = server.Listener.Addr().String()
				return base.RoundTrip(req)
			})
		}))
	require.NoError(t, err)
	_, err = client.Evaluate(t.Context(), "evidence")
	require.NoError(t, err)
	assert.EqualValues(t, 1, requests.Load())
}

type evaluatorRoundTripper func(*http.Request) (*http.Response, error)

func (f evaluatorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGatewayExactEndpoint(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/gateway/development/predict", r.URL.Path)
		assert.Equal(t, "https://private.example.com", r.Header.Get("X-Cagent-Forward"))
		assert.Equal(t, "Bearer docker-token", r.Header.Get("Authorization"))
		_, err := io.WriteString(w, responseBody(`{"type":"noul","noul":1}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	cfg := testConfig("boolean")
	cfg.Endpoint = "https://private.example.com/development/predict"
	cfg.BaseURL = "https://irrelevant.example.com"
	client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{environment.DockerDesktopTokenEnv: "docker-token"}), options.WithGateway(server.URL+"/gateway"))
	require.NoError(t, err)
	_, err = client.Evaluate(t.Context(), "evidence")
	require.NoError(t, err)
}

func TestGatewayFailureNeverCallsUpstream(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("a rejected gateway request fell back to the upstream")
			}))
			t.Cleanup(upstream.Close)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, err := io.WriteString(w, "private-provider-error")
				assert.NoError(t, err)
			}))
			t.Cleanup(gateway.Close)
			cfg := testConfig("boolean")
			cfg.Provider, cfg.BaseURL = backend, upstream.URL
			client, err := New(t.Context(), cfg, environment.NewNoEnvProvider(), options.WithGateway(gateway.URL))
			require.NoError(t, err)
			_, err = client.Evaluate(t.Context(), "private-state")
			require.ErrorContains(t, err, "HTTP status 403")
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestGatewayPreservesEvidenceOnAuthRetry(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, `{"nested":[9007199254740993,18446744073709551615]}`, string(payload["state"])) //nolint:testifylint // JSONEq converts numbers to float64.
		assert.Equal(t, `"encrypted"`, string(payload[httpclient.EncryptedConfigBodyField]))
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, err := io.WriteString(w, responseBody(`{"type":"noul","noul":1}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	var lookups atomic.Int64
	env := environmentFunc(func(context.Context, string) (string, bool) {
		if lookups.Add(1) == 1 {
			return "stale", true
		}
		return "fresh", true
	})
	client, err := New(t.Context(), testConfig("boolean"), env, options.WithGateway(server.URL), options.WithEncryptedConfig("encrypted"))
	require.NoError(t, err)
	_, err = client.Evaluate(t.Context(), json.RawMessage(`{"nested":[9007199254740993,18446744073709551615]}`))
	require.NoError(t, err)
	assert.EqualValues(t, 2, attempts.Load())
}

func TestGatewayEscapedPrefixes(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		for _, prefix := range []string{"/tenant%2Fblue", "/tenant%3Fblue", "/tenant%25blue", "/tenant%2F", "/tenant%3F"} {
			t.Run(backend+prefix, func(t *testing.T) {
				t.Parallel()
				path := "/v1/systemone"
				body := responseBody(`{"type":"noul","noul":1}`)
				if backend == "openai" {
					path, body = "/v1/decisions", openAIResponse(`{"type":"predicate","name":"evaluation","probability":1}`)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, prefix+path+"?tier=paid", r.RequestURI)
					_, err := io.WriteString(w, body)
					assert.NoError(t, err)
				}))
				t.Cleanup(server.Close)
				cfg := testConfig("boolean")
				cfg.Provider = backend
				client, err := New(t.Context(), cfg, environment.NewNoEnvProvider(), options.WithGateway(server.URL+prefix+"/?tier=paid"))
				require.NoError(t, err)
				_, err = client.Evaluate(t.Context(), "state")
				require.NoError(t, err)
			})
		}
	}
}
