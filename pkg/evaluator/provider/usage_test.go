package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

func TestEvaluateUsagePresence(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		usage   string
		want    *evaluator.Usage
		wantErr bool
	}{
		{name: "missing"},
		{name: "null", usage: `null`},
		{name: "empty", usage: `{}`},
		{name: "missing input", usage: `{"output_tokens":1}`},
		{name: "missing output", usage: `{"input_tokens":1}`},
		{name: "null input", usage: `{"input_tokens":null,"output_tokens":0}`},
		{name: "null output", usage: `{"input_tokens":0,"output_tokens":null}`},
		{name: "zero", usage: `{"input_tokens":0,"output_tokens":0}`, want: &evaluator.Usage{}},
		{name: "reported", usage: `{"input_tokens":12,"output_tokens":3}`, want: &evaluator.Usage{InputTokens: 12, OutputTokens: 3}},
		{name: "negative input", usage: `{"input_tokens":-1,"output_tokens":0}`, wantErr: true},
		{name: "negative output", usage: `{"input_tokens":0,"output_tokens":-1}`, wantErr: true},
		{name: "negative partial", usage: `{"input_tokens":-1}`, wantErr: true},
		{name: "string", usage: `"private-state"`, wantErr: true},
		{name: "array", usage: `[]`, wantErr: true},
		{name: "string tokens", usage: `{"input_tokens":"12","output_tokens":0}`, wantErr: true},
		{name: "fraction", usage: `{"input_tokens":1.5,"output_tokens":0}`, wantErr: true},
		{name: "total overflow", usage: `{"input_tokens":9223372036854775807,"output_tokens":1}`, wantErr: true},
		{name: "overflow", usage: `{"input_tokens":9223372036854775808,"output_tokens":0}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := `{"model":"jev-1.13.0","answers":{"evaluation":{"type":"noul","noul":0}}`
			if tt.usage != "" {
				body += `,"usage":` + tt.usage
			}
			body += `}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			cfg := testConfig("boolean")
			cfg.BaseURL = server.URL
			cfg.Cost = &latest.CostConfig{Input: 2, Output: 4}
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "token"}))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			result, err := client.Evaluate(ctx, "state")
			if tt.wantErr {
				require.ErrorContains(t, err, "invalid token usage")
				assert.True(t, evaluator.IsTerminal(err), "invalid usage must not be recoverable")
				assert.NotContains(t, err.Error(), "private-state")
				assert.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
			}
			require.Len(t, records, 1)
			assert.Equal(t, "jev-1.13.0", records[0].Model)
			assert.Equal(t, tt.want, records[0].Usage)
			if tt.want == nil {
				assert.Nil(t, records[0].Cost)
				if result != nil {
					assert.Nil(t, result.Cost)
					assert.Zero(t, result.Usage)
				}
			} else {
				require.NotNil(t, records[0].Cost)
				wantCost := float64(tt.want.InputTokens*2+tt.want.OutputTokens*4) / 1e6
				assert.InDelta(t, wantCost, *records[0].Cost, 1e-12)
				assert.Equal(t, records[0].Cost, result.Cost)
				assert.Equal(t, *tt.want, result.Usage)
			}
		})
	}
}

func TestEstimateCost(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		baseURL   string
		endpoint  string
		requested string
		model     string
		price     *latest.CostConfig
		want      float64
		known     bool
	}{
		{name: "resolved alias", requested: "jev-latest", model: "jev-1.13.0", want: 0.084, known: true},
		{name: "explicit official endpoint", baseURL: defaultBaseURL + "/", model: "jev-1.13.0", want: 0.084, known: true},
		{name: "exact official endpoint", endpoint: defaultBaseURL + "/v1/systemone", model: "jev-1.13.0", want: 0.084, known: true},
		{name: "exact custom endpoint", endpoint: "https://example.com/development/predict", model: "jev-1.13.0"},
		{name: "exact endpoint overrides official base", baseURL: defaultBaseURL, endpoint: "https://example.com/development/predict", model: "jev-1.13.0"},
		{name: "exact endpoint overrides custom base", baseURL: "https://example.com", endpoint: defaultBaseURL + "/v1/systemone", model: "jev-1.13.0", want: 0.084, known: true},
		{name: "exact custom path", endpoint: defaultBaseURL + "/development/predict", model: "jev-1.13.0"},
		{name: "exact lookalike endpoint", endpoint: defaultBaseURL + ".example.com/v1/systemone", model: "jev-1.13.0"},
		{name: "exact endpoint price override", endpoint: "https://example.com/development/predict", model: "laya-rl-agent", price: &latest.CostConfig{Input: 1, Output: 2}, want: 8, known: true},
		{name: "unknown model", model: "jev-future"},
		{name: "unresolved alias", model: "jev-latest"},
		{name: "no requested model fallback", requested: "jev-1.13.0", model: "jev-future"},
		{name: "no response model", requested: "jev-1.13.0"},
		{name: "custom endpoint", baseURL: "https://example.com", model: "jev-1.13.0"},
		{name: "custom path", baseURL: defaultBaseURL + "/proxy", model: "jev-1.13.0"},
		{name: "lookalike endpoint", baseURL: defaultBaseURL + ".example.com", model: "jev-1.13.0"},
		{name: "override", model: "jev-1.13.0", price: &latest.CostConfig{Input: 3, Output: 4}, want: 18, known: true},
		{name: "custom override", baseURL: "https://example.com", model: "unknown", price: &latest.CostConfig{Input: 1, Output: 2}, want: 8, known: true},
		{name: "free", model: "jev-1.13.0", price: &latest.CostConfig{}, known: true},
		{name: "free unknown model", model: "unknown", price: &latest.CostConfig{}, known: true},
		{name: "cache rates unused", price: &latest.CostConfig{CacheRead: 100, CacheWrite: 100}, known: true},
		{name: "overflow", price: &latest.CostConfig{Input: math.MaxFloat64}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig("boolean")
			cfg.BaseURL, cfg.Endpoint, cfg.Cost = tt.baseURL, tt.endpoint, tt.price
			if tt.requested != "" {
				cfg.Model = tt.requested
			}
			client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
			require.NoError(t, err)
			p := client.(*typesafe)
			cost := p.estimateCost(tt.model, &evaluator.Usage{InputTokens: 2_000_000, OutputTokens: 3_000_000})
			if tt.known {
				require.NotNil(t, cost)
				assert.InDelta(t, tt.want, *cost, 1e-12)
				zero := p.estimateCost(tt.model, &evaluator.Usage{})
				require.NotNil(t, zero)
				assert.Zero(t, *zero)
			} else {
				assert.Nil(t, cost)
			}
			assert.Nil(t, p.estimateCost(tt.model, nil))
		})
	}
}

func TestEvaluateObservesFailureUsage(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		body  string
		known bool
	}{
		{name: "invalid answer", body: responseBody(`{"type":"noul","noul":"invalid"}`), known: true},
		{name: "missing answers", body: `{"model":"resolved","usage":{"input_tokens":12,"output_tokens":3}}`, known: true},
		{name: "malformed answers", body: `{"model":"resolved","answers":[],"usage":{"input_tokens":12,"output_tokens":3}}`, known: true},
		{name: "missing model", body: `{"usage":{"input_tokens":12,"output_tokens":3}}`, known: true},
		{name: "malformed model", body: `{"model":123,"usage":{"input_tokens":12,"output_tokens":3}}`, known: true},
		{name: "invalid json", body: `private-state`},
		{name: "partial json", body: `{"usage":{"input_tokens":12,"output_tokens":3},"answers":`},
	} {
		for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
			t.Run(tt.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				t.Parallel()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, tt.body)
				}))
				defer server.Close()
				cfg := testConfig("boolean")
				cfg.BaseURL = server.URL
				cfg.Cost = &latest.CostConfig{Input: 1}
				client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "token"}))
				require.NoError(t, err)
				var records []evaluator.UsageRecord
				ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
				result, err := client.Evaluate(ctx, "state")
				require.Error(t, err)
				assert.Nil(t, result)
				require.Len(t, records, 1)
				if tt.known {
					assert.Equal(t, &evaluator.Usage{InputTokens: 12, OutputTokens: 3}, records[0].Usage)
					require.NotNil(t, records[0].Cost)
					assert.InDelta(t, 12e-6, *records[0].Cost, 1e-12)
				} else {
					assert.Nil(t, records[0].Usage)
					assert.Nil(t, records[0].Cost)
				}
			})
		}
	}
}

func TestEvaluateConcurrentObservers(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request typesafeRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		var state struct{ Tokens int64 }
		if !assert.NoError(t, json.Unmarshal(request.State, &state)) {
			return
		}
		_, _ = fmt.Fprintf(w, `{"model":"jev-1.13.0","answers":{"evaluation":{"type":"noul","noul":0}},"usage":{"input_tokens":%d,"output_tokens":0}}`, state.Tokens)
	}))
	defer server.Close()
	cfg := testConfig("boolean")
	cfg.BaseURL = server.URL
	cfg.Cost = &latest.CostConfig{Input: 2}
	constructionCtx := evaluator.WithUsageObserver(t.Context(), func(evaluator.UsageRecord) {
		t.Error("observer from construction context was retained")
	})
	client, err := New(constructionCtx, cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "token"}))
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			tokens := int64(i + 1)
			result, err := client.Evaluate(ctx, struct{ Tokens int64 }{tokens})
			if assert.NoError(t, err) && assert.Len(t, records, 1) {
				assert.Equal(t, &evaluator.Usage{InputTokens: tokens}, records[0].Usage)
				if assert.NotNil(t, records[0].Cost) {
					assert.InDelta(t, float64(tokens)*2e-6, *records[0].Cost, 1e-12)
				}
				assert.Equal(t, result.Cost, records[0].Cost)
			}
		})
	}
	wg.Wait()
}

func TestNewSnapshotsCost(t *testing.T) {
	t.Parallel()

	cfg := testConfig("boolean")
	cfg.Cost = &latest.CostConfig{Input: 2, Output: 4}
	client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	cfg.Cost.Input, cfg.Cost.Output = 100, 200
	cost := client.(*typesafe).estimateCost("unknown", &evaluator.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	require.NotNil(t, cost)
	assert.InDelta(t, 6, *cost, 1e-12)
}

func TestEvaluateResolvedPricing(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		model string
		price *latest.CostConfig
		known bool
		cost  float64
	}{
		{name: "resolved alias", model: "jev-1.13.0", known: true, cost: 12 * 0.042 / 1e6},
		{name: "unknown model", model: "future"},
		{name: "unresolved alias", model: "jev-latest"},
		{name: "free", model: "future", price: &latest.CostConfig{}, known: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"model":%q,"answers":{"evaluation":{"type":"noul","noul":0}},"usage":{"input_tokens":12,"output_tokens":3}}`, tt.model)
			}))
			defer server.Close()
			cfg := testConfig("boolean")
			cfg.Model, cfg.Cost = "jev-latest", tt.price
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "token"}))
			require.NoError(t, err)
			// Exercise official pricing against a local response, never the public API.
			client.(*typesafe).endpoint = server.URL
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			result, err := client.Evaluate(ctx, "state")
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Equal(t, tt.model, records[0].Model)
			assert.Equal(t, records[0].Cost, result.Cost)
			if tt.known {
				require.NotNil(t, result.Cost)
				assert.InDelta(t, tt.cost, *result.Cost, 1e-12)
			} else {
				assert.Nil(t, result.Cost)
			}
		})
	}
}

func TestEvaluateObservesOnlyAttemptedRequests(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", "token"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig("boolean")
			cfg.BaseURL = "http://127.0.0.1:1"
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": key}))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			_, err = client.Evaluate(ctx, "state")
			require.Error(t, err)
			if key == "" {
				assert.Empty(t, records, "local credential failures are not paid requests")
			} else {
				require.Len(t, records, 1)
				assert.Nil(t, records[0].Usage)
				assert.Nil(t, records[0].Cost)
				assert.Equal(t, cfg.Model, records[0].Model)
			}
		})
	}
}

func TestReportedOpenAIUsage(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		raw     string
		want    *evaluator.Usage
		invalid bool
	}{
		{`{"input_tokens":12}`, &evaluator.Usage{InputTokens: 12}, false},
		{`{"input_tokens":0,"output_tokens":0}`, &evaluator.Usage{}, false},
		{`{"input_tokens":null}`, nil, false},
		{`null`, nil, false},
		{`{}`, nil, false},
		{`{"input_tokens":-1}`, nil, true},
		{`{"input_tokens":12,"output_tokens":-1}`, nil, true},
		{`{"input_tokens":1.5}`, nil, true},
	} {
		usage, err := reportedOpenAIUsage(json.RawMessage(tt.raw))
		if tt.invalid {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		assert.Equal(t, tt.want, usage)
	}
}
