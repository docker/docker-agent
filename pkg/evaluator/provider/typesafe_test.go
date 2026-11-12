package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

func responseBody(answer string) string {
	return `{"model":"jev-resolved","answers":{"evaluation":` + answer + `},"usage":{"input_tokens":12,"output_tokens":3}}`
}

func TestEvaluatePrimitives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		kind       string
		state      any
		answer     string
		question   string
		wantResult string
	}{
		{
			name: "boolean zero", kind: "boolean", state: "plain text",
			answer:     `{"type":"noul","noul":0}`,
			question:   `{"type":"noul","instructions":"Assess the state."}`,
			wantResult: `{"type":"boolean","model":"jev-resolved","probability":0,"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			name: "boolean one", kind: "boolean", state: "",
			answer:     `{"type":"noul","noul":1,"confidence":null}`,
			question:   `{"type":"noul","instructions":"Assess the state."}`,
			wantResult: `{"type":"boolean","model":"jev-resolved","probability":1,"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			name: "choice", kind: "choice", state: map[string]any{"tool": "shell", "args": []string{"echo", "hello"}},
			answer:     `{"type":"choice","choice":"safe","probabilities":{"safe":1,"unsafe":0},"confidence":1}`,
			question:   `{"type":"choice","instructions":"Assess the state.","criteria":{"safe":"Safe to proceed","unsafe":"Do not proceed"}}`,
			wantResult: `{"type":"choice","model":"jev-resolved","choice":"safe","probabilities":{"safe":1,"unsafe":0},"confidence":1,"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			name: "tied choice zero confidence", kind: "choice", state: map[string]any{},
			answer:     `{"type":"choice","choice":"unsafe","probabilities":{"safe":0.5,"unsafe":0.5},"confidence":0}`,
			question:   `{"type":"choice","instructions":"Assess the state.","criteria":{"safe":"Safe to proceed","unsafe":"Do not proceed"}}`,
			wantResult: `{"type":"choice","model":"jev-resolved","choice":"unsafe","probabilities":{"safe":0.5,"unsafe":0.5},"confidence":0,"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			name: "score zero", kind: "score", state: []any{"first", map[string]string{"message": "second"}},
			answer:     `{"type":"score","score":0,"probabilities":{"0":1,"1":0,"2":0},"confidence":0}`,
			question:   `{"type":"score","instructions":"Assess the state.","criteria":["Low","Medium","High"]}`,
			wantResult: `{"type":"score","model":"jev-resolved","score":0,"probabilities":{"0":1,"1":0,"2":0},"confidence":0,"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			name: "score rounded", kind: "score", state: []any{},
			answer:     `{"type":"score","score":1,"probabilities":{"0":0.3333,"1":0.3333,"2":0.3333},"legend":{"0":"Low","1":"Medium","2":"High"}}`,
			question:   `{"type":"score","instructions":"Assess the state.","criteria":["Low","Medium","High"]}`,
			wantResult: `{"type":"score","model":"jev-resolved","score":1,"probabilities":{"0":0.3333,"1":0.3333,"2":0.3333},"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			name: "score maximum", kind: "score", state: json.RawMessage(` ["raw JSON"] `),
			answer:     `{"type":"score","score":2,"probabilities":{"0":0,"1":0,"2":1}}`,
			question:   `{"type":"score","instructions":"Assess the state.","criteria":["Low","Medium","High"]}`,
			wantResult: `{"type":"score","model":"jev-resolved","score":2,"probabilities":{"0":0,"1":0,"2":1},"usage":{"input_tokens":12,"output_tokens":3}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/proxy/v1/systemone", r.URL.Path)
				assert.Equal(t, "Bearer test-secret", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				assert.NotEmpty(t, r.Header.Get("User-Agent"))
				var request typesafeRequest
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
					return
				}
				assert.Equal(t, "jev-test", request.Model)
				assert.Len(t, request.Questions, 1)
				assert.JSONEq(t, tt.question, string(request.Questions["evaluation"]))
				stateJSON, err := json.Marshal(tt.state)
				assert.NoError(t, err)
				assert.JSONEq(t, string(stateJSON), string(request.State))
				_, err = io.WriteString(w, responseBody(tt.answer))
				assert.NoError(t, err)
			}))
			defer server.Close()

			cfg := testConfig(tt.kind)
			cfg.BaseURL = server.URL + "/proxy/"
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "test-secret"}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), tt.state)
			require.NoError(t, err)
			require.NotNil(t, result)
			resultJSON, err := json.Marshal(result)
			require.NoError(t, err)
			assert.JSONEq(t, tt.wantResult, string(resultJSON))
			assert.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestEvaluateRejectsInvalidAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		kind   string
		answer string
	}{
		{"null answer", "boolean", `null`},
		{"empty answer", "boolean", `{}`},
		{"missing type", "boolean", `{"noul":0.5}`},
		{"wrong type", "boolean", `{"type":"choice","noul":0.5}`},
		{"missing noul", "boolean", `{"type":"noul"}`},
		{"null noul", "boolean", `{"type":"noul","noul":null}`},
		{"string noul", "boolean", `{"type":"noul","noul":"private-state"}`},
		{"negative noul", "boolean", `{"type":"noul","noul":-0.1}`},
		{"large noul", "boolean", `{"type":"noul","noul":1.1}`},
		{"overflow noul", "boolean", `{"type":"noul","noul":1e400}`},
		{"bad confidence", "boolean", `{"type":"noul","noul":0.5,"confidence":-0.1}`},
		{"large confidence", "boolean", `{"type":"noul","noul":0.5,"confidence":1.1}`},
		{"missing choice", "choice", `{"type":"choice","probabilities":{"safe":1,"unsafe":0}}`},
		{"null choice", "choice", `{"type":"choice","choice":null,"probabilities":{"safe":1,"unsafe":0}}`},
		{"unknown choice", "choice", `{"type":"choice","choice":"private-state","probabilities":{"safe":1,"unsafe":0}}`},
		{"not highest choice", "choice", `{"type":"choice","choice":"unsafe","probabilities":{"safe":0.8,"unsafe":0.2}}`},
		{"missing probabilities", "choice", `{"type":"choice","choice":"safe"}`},
		{"null probabilities", "choice", `{"type":"choice","choice":"safe","probabilities":null}`},
		{"missing key", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":1}}`},
		{"extra key", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":1,"unsafe":0,"extra":0}}`},
		{"replaced key", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":1,"extra":0}}`},
		{"null probability", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":1,"unsafe":null}}`},
		{"negative probability", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":1,"unsafe":-0.1}}`},
		{"large probability", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":1.1,"unsafe":0}}`},
		{"bad sum", "choice", `{"type":"choice","choice":"safe","probabilities":{"safe":0.5,"unsafe":0.1}}`},
		{"missing score", "score", `{"type":"score","probabilities":{"0":1,"1":0,"2":0}}`},
		{"null score", "score", `{"type":"score","score":null,"probabilities":{"0":1,"1":0,"2":0}}`},
		{"negative score", "score", `{"type":"score","score":-1,"probabilities":{"0":1,"1":0,"2":0}}`},
		{"large score", "score", `{"type":"score","score":3,"probabilities":{"0":0,"1":0,"2":1}}`},
		{"string score", "score", `{"type":"score","score":"private-state","probabilities":{"0":1,"1":0,"2":0}}`},
		{"score level names", "score", `{"type":"score","score":1,"probabilities":{"Low":0,"Medium":1,"High":0}}`},
		{"score wrong indices", "score", `{"type":"score","score":1,"probabilities":{"1":1,"2":0,"3":0}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, responseBody(tt.answer))
				assert.NoError(t, err)
			}))
			defer server.Close()
			cfg := testConfig(tt.kind)
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token"}))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) {
				records = append(records, record)
			})
			result, err := client.Evaluate(ctx, "private-state")
			require.Error(t, err)
			assert.Nil(t, result)
			require.Len(t, records, 1)
			assert.Equal(t, "jev-resolved", records[0].Model)
			assert.Equal(t, &evaluator.Usage{InputTokens: 12, OutputTokens: 3}, records[0].Usage)
			assert.NotContains(t, err.Error(), "private-state")
			assert.NotContains(t, err.Error(), "private-token")
		})
	}
}

func TestEvaluateInvalidResponses(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"invalid json": "private-state", "null response": "null", "empty object": "{}",
		"missing answer": `{"model":"jev","answers":{"other":{"type":"noul","noul":0}}}`,
		"missing model":  `{"answers":{"evaluation":{"type":"noul","noul":0}}}`,
		"trailing json":  responseBody(`{"type":"noul","noul":0}`) + `{}`,
		"negative usage": `{"model":"jev","answers":{"evaluation":{"type":"noul","noul":0}},"usage":{"input_tokens":-1}}`,
		"oversized":      strings.Repeat(" ", maxResponseBytes) + responseBody(`{"type":"noul","noul":0}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			cfg := testConfig("boolean")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token"}))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			result, err := client.Evaluate(ctx, "private-state")
			require.Error(t, err)
			assert.Nil(t, result)
			require.Len(t, records, 1)
			assert.Nil(t, records[0].Usage)
			assert.Nil(t, records[0].Cost)
			assert.NotContains(t, err.Error(), "private-state")
		})
	}
}

func TestScoreAllowsBoundaryRounding(t *testing.T) {
	t.Parallel()

	for _, score := range []float64{-0.0001, 2.0001} {
		t.Run(strconv.FormatFloat(score, 'g', -1, 64), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, responseBody(fmt.Sprintf(`{"type":"score","score":%g,"probabilities":{"0":0,"1":0,"2":1}}`, score)))
				assert.NoError(t, err)
			}))
			defer server.Close()
			cfg := testConfig("score")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "token"}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), "state")
			require.NoError(t, err)
			require.NotNil(t, result.Score)
			assert.InDelta(t, score, *result.Score, 1e-10)
		})
	}
}

func TestEvaluateTruncatedResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, err := io.WriteString(w, "private-state")
		assert.NoError(t, err)
	}))
	defer server.Close()
	cfg := testConfig("boolean")
	cfg.BaseURL = server.URL
	client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "token"}))
	require.NoError(t, err)
	var records []evaluator.UsageRecord
	ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
	result, err := client.Evaluate(ctx, "state")
	require.ErrorContains(t, err, "failed to read evaluator response")
	assert.NotContains(t, err.Error(), "private-state")
	assert.Nil(t, result)
	require.Len(t, records, 1)
	assert.Nil(t, records[0].Usage)
	assert.Nil(t, records[0].Cost)
}

func TestEvaluateHTTPFailures(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNoContent, http.StatusUnauthorized, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-state private-token")
			}))
			defer server.Close()
			cfg := testConfig("boolean")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token"}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), "private-state")
			require.ErrorContains(t, err, strconv.Itoa(status))
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), "private-state")
			assert.NotContains(t, err.Error(), "private-token")
		})
	}
}

func TestEvaluateDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/private-state", status)
			}))
			defer server.Close()
			cfg := testConfig("boolean")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token"}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), "private-state")
			require.Error(t, err)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), "private-state")
		})
	}
	assert.Zero(t, redirected.Load())
}

func TestEvaluateCancellationAndTimeout(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"headers", "body"} {
		for _, cancelRequest := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cancel=%t", phase, cancelRequest), func(t *testing.T) {
				t.Parallel()
				started := make(chan struct{})
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if phase == "body" {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer server.Close()
				defer close(release)
				cfg := testConfig("boolean")
				cfg.BaseURL = server.URL
				if !cancelRequest {
					cfg.Timeout.Duration = 100 * time.Millisecond
				}
				client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token"}))
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := client.Evaluate(ctx, "private-state")
					done <- err
				}()
				if cancelRequest {
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						t.Fatal("request did not reach test server")
					}
					cancel()
				}
				select {
				case err := <-done:
					if cancelRequest {
						require.ErrorIs(t, err, context.Canceled)
					} else {
						require.ErrorIs(t, err, context.DeadlineExceeded)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("evaluation ignored cancellation or timeout")
				}
			})
		}
	}
}

func TestEvaluateConcurrentRequestCredentials(t *testing.T) {
	t.Parallel()

	type credentialKey struct{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request typesafeRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		var credential string
		if !assert.NoError(t, json.Unmarshal(request.State, &credential)) {
			return
		}
		assert.Equal(t, "Bearer "+credential, r.Header.Get("Authorization"))
		_, err := io.WriteString(w, responseBody(`{"type":"noul","noul":0}`))
		assert.NoError(t, err)
	}))
	defer server.Close()
	cfg := testConfig("boolean")
	cfg.BaseURL = server.URL
	cfg.TokenKey = "SESSION_TOKEN"
	client, err := New(t.Context(), cfg, environmentFunc(func(ctx context.Context, name string) (string, bool) {
		assert.Equal(t, "SESSION_TOKEN", name)
		value, ok := ctx.Value(credentialKey{}).(string)
		return value, ok
	}))
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			credential := fmt.Sprintf("session-%d", i)
			ctx := context.WithValue(t.Context(), credentialKey{}, credential)
			result, err := client.Evaluate(ctx, credential)
			if assert.NoError(t, err) {
				assert.NotNil(t, result)
			}
		})
	}
	wg.Wait()
}

func TestNewSnapshotsCriteria(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"choice", "score"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(kind)
			client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
			require.NoError(t, err)
			p := client.(*typesafe)
			before := string(p.question)
			if kind == "choice" {
				cfg.Choices["safe"] = "changed"
				delete(cfg.Choices, "unsafe")
			} else {
				cfg.Levels[0] = "changed"
			}
			assert.Equal(t, before, string(p.question))
		})
	}
}

func TestEvaluateTransportErrorRedaction(t *testing.T) {
	t.Parallel()

	cfg := testConfig("boolean")
	cfg.BaseURL = "http://127.0.0.1:1/private-state"
	client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "private-token\n"}))
	require.NoError(t, err)
	result, err := client.Evaluate(t.Context(), "private-state")
	require.Error(t, err)
	assert.Nil(t, result)
	assert.NotContains(t, err.Error(), "private-state")
	assert.NotContains(t, err.Error(), "private-token")
}
