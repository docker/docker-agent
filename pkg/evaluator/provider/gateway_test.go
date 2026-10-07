package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/httpclient"
)

type gatewayRequest struct {
	path, query, auth, forward, provider, model, session string
	header                                               http.Header
}

type gatewayLog struct {
	mu       sync.Mutex
	requests []gatewayRequest
}

func (l *gatewayLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, gatewayRequest{
		path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
		forward: r.Header.Get("X-Cagent-Forward"), provider: r.Header.Get("X-Cagent-Provider"),
		model: r.Header.Get("X-Cagent-Model"), session: r.Header.Get("X-Cagent-Session-Id"),
		header: r.Header.Clone(),
	})
}

func (l *gatewayLog) all() []gatewayRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

// gatewayServer plays a models gateway on loopback, which trusts Docker tokens.
// respond chooses each reply from the request number, starting at 1.
func gatewayServer(t *testing.T, respond func(n int, r *http.Request) (int, string)) (*httptest.Server, *gatewayLog) {
	t.Helper()
	log := &gatewayLog{}
	var n atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		status, body := respond(int(n.Add(1)), r)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, log
}

func okGateway(answer string) func(int, *http.Request) (int, string) {
	return func(int, *http.Request) (int, string) { return http.StatusOK, answer }
}

const typesafeBoolean = `{"model":"jev-resolved","answers":{"evaluation":{"type":"noul","noul":1}},"usage":{"input_tokens":12,"output_tokens":3}}`

func dockerToken(token string) environment.Provider {
	return environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: token, "OPENAI_API_KEY": "upstream-openai", "TYPESAFE_API_KEY": "upstream-typesafe",
	})
}

func gatewayConfig(provider string) latest.EvaluatorConfig {
	if provider == "openai" {
		return openaiConfig("boolean")
	}
	return testConfig("boolean")
}

func answerFor(provider string) string {
	if provider == "openai" {
		return decisionsBody(predicateOne, syntheticUsage)
	}
	return typesafeBoolean
}

func TestGatewayPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, prefix, want string
	}{
		{"openai", "", "/v1/decisions"},
		{"openai", "/gateway", "/gateway/v1/decisions"},
		{"openai", "/gateway/", "/gateway/v1/decisions"},
		{"openai", "/a/b", "/a/b/v1/decisions"},
		{"typesafe", "", "/v1/systemone"},
		{"typesafe", "/gateway", "/gateway/v1/systemone"},
		{"typesafe", "/gateway/", "/gateway/v1/systemone"},
	} {
		t.Run(tc.provider+tc.prefix, func(t *testing.T) {
			t.Parallel()
			server, log := gatewayServer(t, okGateway(answerFor(tc.provider)))
			client, err := New(t.Context(), gatewayConfig(tc.provider), dockerToken("docker-token"), WithModelsGateway(server.URL+tc.prefix+"?tenant=a"))
			require.NoError(t, err)
			_, err = client.Evaluate(httpclient.ContextWithSessionID(t.Context(), "session-1"), "state")
			require.NoError(t, err)
			require.Len(t, log.all(), 1)
			got := log.all()[0]
			assert.Equal(t, tc.want, got.path)
			assert.Equal(t, "tenant=a", got.query, "gateway query options are kept")
			assert.Equal(t, "Bearer docker-token", got.auth, "the Docker token, never the upstream key")
			assert.Equal(t, tc.provider, got.provider)
			assert.Equal(t, gatewayConfig(tc.provider).Model, got.model)
			assert.Equal(t, "session-1", got.session)
			if tc.provider == "openai" {
				assert.Equal(t, "https://api.openai.com/v1", got.forward)
			} else {
				assert.Equal(t, "https://api.typesafe.ai", got.forward)
			}
			for name, values := range got.header {
				for _, value := range values {
					assert.NotContains(t, value, "upstream-", name)
				}
			}
		})
	}
}

func TestGatewayAuthentication(t *testing.T) {
	t.Parallel()
	t.Run("loopback without a Desktop token sends none", func(t *testing.T) {
		t.Parallel()
		server, log := gatewayServer(t, okGateway(answerFor("openai")))
		client, err := New(t.Context(), gatewayConfig("openai"), environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "upstream-openai"}), WithModelsGateway(server.URL))
		require.NoError(t, err)
		_, err = client.Evaluate(t.Context(), "state")
		require.NoError(t, err)
		assert.Empty(t, log.all()[0].auth)
	})
	t.Run("Docker domain requires a token", func(t *testing.T) {
		t.Parallel()
		var requests atomic.Int64
		wrap := func(http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("must not be reached")
			})
		}
		client, err := New(t.Context(), gatewayConfig("openai"), environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "upstream-openai"}),
			WithModelsGateway("https://gateway.docker.com"), WithHTTPTransportWrapper(wrap))
		require.NoError(t, err, "no authentication happens at construction")
		ctx := evaluator.WithUsageObserver(t.Context(), func(evaluator.UsageRecord) { t.Error("no request was attempted") })
		_, err = client.Evaluate(ctx, "state")
		require.ErrorContains(t, err, "authentication failed")
		assert.Zero(t, requests.Load())
	})
	t.Run("untrusted gateways get no Docker token and no provider key", func(t *testing.T) {
		t.Parallel()
		var got *http.Request
		wrap := func(http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				got = r
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answerFor("openai"))), Header: http.Header{}}, nil
			})
		}
		client, err := New(t.Context(), gatewayConfig("openai"), dockerToken("docker-token"),
			WithModelsGateway("https://models.example.com/gw"), WithHTTPTransportWrapper(wrap))
		require.NoError(t, err)
		_, err = client.Evaluate(t.Context(), "state")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "https://models.example.com/gw/v1/decisions", got.URL.String())
		assert.Empty(t, got.Header.Get("Authorization"))
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayBypassAndCustomEndpointsStayDirect(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*latest.EvaluatorConfig, string){
		"bypass":   func(c *latest.EvaluatorConfig, direct string) { c.BypassModelsGateway, c.BaseURL = true, direct },
		"base url": func(c *latest.EvaluatorConfig, direct string) { c.BaseURL = direct },
		"endpoint": func(c *latest.EvaluatorConfig, direct string) { c.Endpoint = direct + "/exact" },
	} {
		for _, provider := range []string{"openai", "typesafe"} {
			t.Run(name+"/"+provider, func(t *testing.T) {
				t.Parallel()
				gateway, gatewayLog := gatewayServer(t, okGateway(answerFor(provider)))
				direct, directLog := gatewayServer(t, okGateway(answerFor(provider)))
				cfg := gatewayConfig(provider)
				edit(&cfg, direct.URL)
				client, err := New(t.Context(), cfg, dockerToken("docker-token"), WithModelsGateway(gateway.URL))
				require.NoError(t, err)
				_, err = client.Evaluate(t.Context(), "state")
				require.NoError(t, err)
				assert.Empty(t, gatewayLog.all())
				require.Len(t, directLog.all(), 1)
				got := directLog.all()[0]
				assert.Equal(t, "Bearer upstream-"+provider, got.auth, "the provider key, never the Docker token")
				assert.Empty(t, got.forward, "no gateway routing headers")
			})
		}
	}
	t.Run("bypass alone reaches the native API", func(t *testing.T) {
		t.Parallel()
		var target string
		wrap := func(http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				target = r.URL.String()
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answerFor("openai"))), Header: http.Header{}}, nil
			})
		}
		cfg := openaiConfig("boolean")
		cfg.BypassModelsGateway = true
		client, err := New(t.Context(), cfg, dockerToken("docker-token"), WithModelsGateway("https://models.example.com"), WithHTTPTransportWrapper(wrap))
		require.NoError(t, err)
		_, err = client.Evaluate(t.Context(), "state")
		require.NoError(t, err)
		assert.Equal(t, "https://api.openai.com/v1/decisions", target)
	})
}

func TestGatewayRejectsAmbiguousPaths(t *testing.T) {
	t.Parallel()
	for name, gateway := range map[string]string{
		"dot dot":         "https://gw.example.com/a/../b",
		"dot":             "https://gw.example.com/a/./b",
		"trailing dot":    "https://gw.example.com/a/..",
		"encoded":         "https://gw.example.com/a/%2e%2e/b",
		"upper encoded":   "https://gw.example.com/a/%2E%2E",
		"double encoded":  "https://gw.example.com/a/%252e%252e/b",
		"encoded slash":   "https://gw.example.com/a%2f..%2fb",
		"encoded bslash":  "https://gw.example.com/a%5c..%5cb",
		"credentials":     "https://user:secret@gw.example.com",
		"relative":        "gw.example.com/path",
		"unsupported":     "ftp://gw.example.com",
		"fragment":        "https://gw.example.com/#x",
		"invalid escaped": "https://gw.example.com/%zz",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, cfg := range []latest.EvaluatorConfig{openaiConfig("boolean"), testConfig("boolean")} {
				client, err := New(t.Context(), cfg, dockerToken("t"), WithModelsGateway(gateway))
				require.Error(t, err)
				assert.Nil(t, client)
				assert.NotContains(t, err.Error(), "secret")
			}
		})
	}
	t.Run("ignored when the evaluator dials directly", func(t *testing.T) {
		t.Parallel()
		cfg := openaiConfig("boolean")
		cfg.BypassModelsGateway = true
		_, err := New(t.Context(), cfg, dockerToken("t"), WithModelsGateway("https://gw.example.com/a/../b"))
		require.NoError(t, err)
	})
}

func TestGatewayRejectsInvalidPathsWithMockGateway(t *testing.T) {
	t.Parallel()
	// A gateway that refuses traversal is never asked to: construction fails first.
	server, log := gatewayServer(t, func(_ int, r *http.Request) (int, string) {
		if strings.Contains(r.URL.Path, "..") {
			return http.StatusBadRequest, ""
		}
		return http.StatusOK, answerFor("openai")
	})
	_, err := New(t.Context(), openaiConfig("boolean"), dockerToken("t"), WithModelsGateway(server.URL+"/%2e%2e/admin"))
	require.Error(t, err)
	assert.Empty(t, log.all())
}

func TestGatewayAdmissionAndSanitizedErrors(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"openai", "typesafe"} {
		for name, tc := range map[string]struct {
			status int
			body   string
		}{
			"admission":       {http.StatusBadRequest, `{"error":"definition rejected"}`},
			"empty 500":       {http.StatusInternalServerError, ``},
			"text 502":        {http.StatusBadGateway, `response replaced`},
			"json 502":        {http.StatusBadGateway, `{"error":{"type":"gateway_error"}}`},
			"oversized 502":   {http.StatusBadGateway, strings.Repeat("y", maxResponseBytes+10)},
			"unauthenticated": {http.StatusForbidden, `{"error":"no capability"}`},
		} {
			t.Run(provider+"/"+name, func(t *testing.T) {
				t.Parallel()
				direct, directLog := gatewayServer(t, okGateway(answerFor(provider)))
				server, log := gatewayServer(t, func(int, *http.Request) (int, string) { return tc.status, tc.body })
				cfg := gatewayConfig(provider)
				client, err := New(t.Context(), cfg, dockerToken("docker-token"), WithModelsGateway(server.URL))
				require.NoError(t, err)
				ctx, records := observe(t.Context())
				result, err := client.Evaluate(ctx, "state")
				require.ErrorContains(t, err, fmt.Sprintf("HTTP status %d", tc.status))
				assert.Nil(t, result)
				assert.False(t, evaluator.IsTerminal(err))
				assert.Len(t, log.all(), 1, "no retry")
				assert.Empty(t, directLog.all(), "no fallback to a direct connection")
				_ = direct
				require.Len(t, records(), 1)
				assert.Nil(t, records()[0].Usage)
			})
		}
	}
}

// rotatingEnv hands out a new Docker token after each rejection.
type rotatingEnv struct {
	mu     sync.Mutex
	tokens []string
	gets   int
}

func (e *rotatingEnv) Get(_ context.Context, name string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if name != environment.DockerDesktopTokenEnv {
		return "", false
	}
	token := e.tokens[min(e.gets, len(e.tokens)-1)]
	e.gets++
	return token, true
}

func TestGatewayAuthenticationRefresh(t *testing.T) {
	t.Parallel()
	ok := answerFor("openai")
	for name, tc := range map[string]struct {
		statuses      []int
		tokens        []string
		wantRequests  int
		wantAuth      []string
		wantErr       string
		wantRecords   int
		wantFinalData bool
	}{
		"rejected then accepted":    {[]int{401, 200}, []string{"stale-a", "fresh-a"}, 2, []string{"Bearer stale-a", "Bearer fresh-a"}, "", 2, true},
		"rejected twice":            {[]int{401, 401, 200}, []string{"stale-b", "fresh-b"}, 2, []string{"Bearer stale-b", "Bearer fresh-b"}, "HTTP status 401", 2, false},
		"static token is not retry": {[]int{401, 200}, []string{"static-c"}, 1, []string{"Bearer static-c"}, "HTTP status 401", 1, false},
		"other statuses not replay": {[]int{403, 200}, []string{"stale-d", "fresh-d"}, 1, []string{"Bearer stale-d"}, "HTTP status 403", 1, false},
		"server errors not replay":  {[]int{500, 200}, []string{"stale-e", "fresh-e"}, 1, []string{"Bearer stale-e"}, "HTTP status 500", 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, log := gatewayServer(t, func(n int, _ *http.Request) (int, string) {
				status := tc.statuses[min(n-1, len(tc.statuses)-1)]
				if status == http.StatusOK {
					return status, ok
				}
				return status, ``
			})
			client, err := New(t.Context(), gatewayConfig("openai"), &rotatingEnv{tokens: tc.tokens}, WithModelsGateway(server.URL))
			require.NoError(t, err)
			ctx, records := observe(t.Context())
			result, err := client.Evaluate(ctx, "state")
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, &evaluator.Usage{InputTokens: 160}, &result.Usage)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			require.Len(t, log.all(), tc.wantRequests)
			for i, auth := range tc.wantAuth {
				assert.Equal(t, auth, log.all()[i].auth)
			}
			got := records()
			require.Len(t, got, tc.wantRecords, "each attempt is accounted exactly once")
			if tc.wantRecords == 2 {
				assert.Nil(t, got[0].Usage, "the rejected attempt has unknown usage")
				assert.Equal(t, "gpt-6-luna", got[0].Model)
			}
			if tc.wantFinalData {
				assert.Equal(t, &evaluator.Usage{InputTokens: 160}, got[1].Usage)
			}
		})
	}
}

func TestGatewayRefreshFailureAndCancellation(t *testing.T) {
	t.Parallel()
	t.Run("refresh failure", func(t *testing.T) {
		t.Parallel()
		server, log := gatewayServer(t, func(int, *http.Request) (int, string) { return http.StatusUnauthorized, "" })
		env := environmentFunc(func(_ context.Context, name string) (string, bool) {
			if name == environment.DockerDesktopTokenEnv {
				return "stale-f", true
			}
			return "", false
		})
		client, err := New(t.Context(), gatewayConfig("openai"), env, WithModelsGateway(server.URL))
		require.NoError(t, err)
		ctx, records := observe(t.Context())
		_, err = client.Evaluate(ctx, "state")
		require.ErrorContains(t, err, "HTTP status 401")
		assert.Len(t, log.all(), 1)
		assert.Len(t, records(), 1)
	})
	t.Run("canceled during refresh", func(t *testing.T) {
		t.Parallel()
		server, log := gatewayServer(t, func(int, *http.Request) (int, string) { return http.StatusUnauthorized, "" })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var gets atomic.Int64
		env := environmentFunc(func(_ context.Context, name string) (string, bool) {
			if name != environment.DockerDesktopTokenEnv {
				return "", false
			}
			if gets.Add(1) > 1 {
				cancel()
				return "fresh-g", true
			}
			return "stale-g", true
		})
		client, err := New(t.Context(), gatewayConfig("openai"), env, WithModelsGateway(server.URL))
		require.NoError(t, err)
		ctx, records := observe(ctx)
		_, err = client.Evaluate(ctx, "state")
		require.ErrorIs(t, err, context.Canceled)
		assert.Len(t, log.all(), 1, "a canceled evaluation is not replayed")
		assert.Len(t, records(), 1)
	})
}

func TestGatewayConcurrentSessionsShareOneClient(t *testing.T) {
	t.Parallel()
	server, log := gatewayServer(t, okGateway(answerFor("openai")))
	client, err := New(t.Context(), gatewayConfig("openai"), dockerToken("docker-token"), WithModelsGateway(server.URL))
	require.NoError(t, err)

	const sessions = 16
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Go(func() {
			ctx, records := observe(httpclient.ContextWithSessionID(t.Context(), fmt.Sprintf("session-%d", i)))
			_, err := client.Evaluate(ctx, "state")
			require.NoError(t, err)
			assert.Len(t, records(), 1, "observers see only their own request")
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, r := range log.all() {
		seen[r.session] = true
	}
	assert.Len(t, seen, sessions)
}

func TestGatewayTypeSafeAccountingWithoutOfficialPricing(t *testing.T) {
	t.Parallel()
	server, _ := gatewayServer(t, okGateway(strings.Replace(typesafeBoolean, "jev-resolved", "jev-1.13.0", 1)))
	client, err := New(t.Context(), gatewayConfig("typesafe"), dockerToken("docker-token"), WithModelsGateway(server.URL))
	require.NoError(t, err)
	result, err := client.Evaluate(t.Context(), "state")
	require.NoError(t, err)
	assert.Nil(t, result.Cost, "gateway billing is not reconstructed from public rates")
	assert.Equal(t, evaluator.Usage{InputTokens: 12, OutputTokens: 3}, result.Usage)
}

func TestTransportWrapperApplies(t *testing.T) {
	t.Parallel()
	var wrapped atomic.Int64
	server, _ := gatewayServer(t, okGateway(answerFor("openai")))
	cfg := openaiConfig("boolean")
	cfg.BaseURL = server.URL
	client, err := New(t.Context(), cfg, openaiKey, WithHTTPTransportWrapper(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			wrapped.Add(1)
			return rt.RoundTrip(r)
		})
	}), nil)
	require.NoError(t, err)
	_, err = client.Evaluate(t.Context(), "state")
	require.NoError(t, err)
	assert.EqualValues(t, 1, wrapped.Load())
}
