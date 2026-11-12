package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

func TestNewExactEndpoint(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"https://example.com/development/predict",
		"https://example.com/development/predict/",
		"https://example.com/custom%2Fpath",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig("boolean")
			cfg.BaseURL = "https://ignored.example.com"
			cfg.Endpoint = endpoint
			client, err := New(t.Context(), cfg, environmentFunc(func(context.Context, string) (string, bool) {
				t.Error("construction must not look up credentials")
				return "", false
			}))
			require.NoError(t, err)
			p := client.(*typesafe)
			assert.Equal(t, endpoint, p.endpoint)
			assert.False(t, p.officialPricing)
			assert.ErrorIs(t, p.client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
		})
	}
}

func TestNewRejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"relative/path", "ftp://example.com", "https://", " ",
		"https://user:private-token@example.com/predict", "https://example.com/predict?private-token",
		"https://example.com/predict?", "https://example.com/predict#private-token", "https://example.com/%",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig("boolean")
			cfg.Endpoint = endpoint
			client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-token")
			assert.Nil(t, client)
		})
	}
}

func TestEvaluateLayaEndpoint(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		kind   string
		answer string
	}{
		{"boolean", `{"type":"noul","noul":0.8924,"confidence":0.8924}`},
		{"choice", `{"type":"choice","choice":"billing","probabilities":{"billing":0.9173,"technical":0.0827},"confidence":0.5882}`},
		{"score", `{"type":"score","score":1.2204,"probabilities":{"0":0.0544,"1":0.6709,"2":0.2747},"confidence":0.289}`},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int64
			cfg := testConfig(tt.kind)
			cfg.Model = "english"
			cfg.TokenKey = "BASETEN_API_KEY"
			if tt.kind == "choice" {
				cfg.Choices = map[string]string{"billing": "Invoices, payments, and refunds.", "technical": "Software bugs and technical support."}
			}
			state := map[string]string{"message": "Please refund the duplicate invoice charge."}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/development/predict", r.RequestURI)
				assert.Equal(t, "Bearer test-secret", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				var request typesafeRequest
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
					return
				}
				assert.Equal(t, "english", request.Model)
				assert.JSONEq(t, `{"message":"Please refund the duplicate invoice charge."}`, string(request.State))
				assert.Len(t, request.Questions, 1)
				var question typesafeQuestion
				assert.NoError(t, json.Unmarshal(request.Questions["evaluation"], &question))
				kind := tt.kind
				if kind == "boolean" {
					kind = "noul"
				}
				assert.Equal(t, kind, question.Type)
				_, err := io.WriteString(w, `{"model":"laya-rl-agent","answers":{"evaluation":`+tt.answer+`},"usage":{"input_tokens":45,"output_tokens":0},"routing":{"model":"english"}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			cfg.Endpoint = server.URL + "/development/predict"
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"BASETEN_API_KEY": "test-secret"}))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			result, err := client.Evaluate(ctx, state)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, "laya-rl-agent", result.Model)
			assert.Equal(t, tt.kind, result.Type)
			assert.Equal(t, evaluator.Usage{InputTokens: 45}, result.Usage)
			assert.Nil(t, result.Cost)
			switch tt.kind {
			case "boolean":
				require.NotNil(t, result.Probability)
				assert.InDelta(t, 0.8924, *result.Probability, 1e-9)
			case "choice":
				assert.Equal(t, "billing", result.Choice)
				assert.Equal(t, map[string]float64{"billing": 0.9173, "technical": 0.0827}, result.Probabilities)
			case "score":
				require.NotNil(t, result.Score)
				assert.InDelta(t, 1.2204, *result.Score, 1e-9)
			}
			require.Len(t, records, 1)
			assert.Equal(t, result.Model, records[0].Model)
			assert.Equal(t, &result.Usage, records[0].Usage)
			assert.Nil(t, records[0].Cost)
			assert.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestEvaluateExactEndpointFailures(t *testing.T) {
	t.Parallel()

	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	t.Cleanup(target.Close)
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", http.StatusTemporaryRedirect, "private-token private-state"},
		{"server error", http.StatusInternalServerError, "private-token private-state"},
		{"invalid JSON", http.StatusOK, "private-token private-state"},
		{"missing model", http.StatusOK, `{"answers":{"evaluation":{"type":"noul","noul":0.5}}}`},
		{"invalid answer", http.StatusOK, `{"model":"laya-rl-agent","answers":{"evaluation":{"type":"noul","noul":2}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/predict", r.RequestURI)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			cfg := testConfig("boolean")
			cfg.Endpoint = server.URL + "/predict"
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token"}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), "private-state")
			require.Error(t, err)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), "private-token")
			assert.NotContains(t, err.Error(), "private-state")
		})
	}
	assert.Zero(t, redirected.Load())
}
