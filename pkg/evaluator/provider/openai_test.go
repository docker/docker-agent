package provider

import (
	"context"
	"encoding/json"
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
)

func openaiConfig(kind string) latest.EvaluatorConfig {
	cfg := testConfig(kind)
	cfg.Provider, cfg.Model = "openai", "gpt-6-luna"
	return cfg
}

var openaiKey = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "private-token"})

const (
	syntheticUsage = `"usage":{"input_tokens":160,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":160}`
	predicateOne   = `{"type":"predicate","name":"evaluation","probability":1.0}`
)

func decisionsBody(answers string, extra ...string) string {
	return `{"model":"gpt-6-luna","answers":[` + answers + `]` + strings.Join(append([]string{""}, extra...), ",") + "}"
}

type captured struct {
	method, path, auth string
	body               map[string]any
}

// decisionsServer answers every request with body and records the first.
func decisionsServer(t *testing.T, status int, body string) (*httptest.Server, *captured, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	got := &captured{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			got.method, got.path, got.auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got.body))
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, got, &requests
}

func observe(ctx context.Context) (context.Context, func() []evaluator.UsageRecord) {
	var mu sync.Mutex
	var records []evaluator.UsageRecord
	ctx = evaluator.WithUsageObserver(ctx, func(r evaluator.UsageRecord) {
		mu.Lock()
		defer mu.Unlock()
		records = append(records, r)
	})
	return ctx, func() []evaluator.UsageRecord {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(records)
	}
}

func TestOpenAIDefaults(t *testing.T) {
	t.Parallel()
	env := environmentFunc(func(context.Context, string) (string, bool) {
		t.Error("credentials must not be resolved when the client is constructed")
		return "", false
	})
	client, err := New(t.Context(), openaiConfig("boolean"), env)
	require.NoError(t, err)
	p := client.(*openai)
	assert.Equal(t, "https://api.openai.com/v1/decisions", p.endpoint)
	assert.Equal(t, "OPENAI_API_KEY", p.tokenKey)
	assert.Empty(t, p.gateway)
	assert.ErrorIs(t, p.client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
}

func TestOpenAIEndpoints(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		base, endpoint, want string
	}{
		"api base":           {base: "https://proxy.example.com/api/v1/", want: "https://proxy.example.com/api/v1/decisions"},
		"api base no slash":  {base: "https://proxy.example.com/api/v1", want: "https://proxy.example.com/api/v1/decisions"},
		"arbitrary path":     {base: "https://proxy.example.com/custom", want: "https://proxy.example.com/custom/decisions"},
		"exact endpoint":     {base: "https://proxy.example.com/v1", endpoint: "https://other.example.com/x", want: "https://other.example.com/x"},
		"endpoint without /": {endpoint: "https://other.example.com/x/", want: "https://other.example.com/x/"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := openaiConfig("boolean")
			cfg.BaseURL, cfg.Endpoint = tc.base, tc.endpoint
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			assert.Equal(t, tc.want, client.(*openai).endpoint)
		})
	}
	cfg := openaiConfig("boolean")
	cfg.Endpoint = "https://user:secret@example.com/x"
	_, err := New(t.Context(), cfg, openaiKey)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
}

func TestOpenAIRequestMapping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		kind     string
		state    any
		input    string
		question string
		answer   string
	}{
		{
			name: "boolean string", kind: "boolean", state: "plain \"quoted\" text", input: `plain "quoted" text`,
			question: `{"type":"predicate","name":"evaluation","instructions":"Assess the state."}`,
			answer:   predicateOne,
		},
		{
			name: "choice object", kind: "choice", state: map[string]any{"b": 1, "a": []string{"x"}}, input: `{"a":["x"],"b":1}`,
			question: `{"type":"choice","name":"evaluation","instructions":"Assess the state.","choices":[{"value":"safe","description":"Safe to proceed"},{"value":"unsafe","description":"Do not proceed"}]}`,
			answer:   `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"unsafe","probability":0},{"value":"safe","probability":1}]}`,
		},
		{
			name: "score array", kind: "score", state: []any{"first", 2}, input: `["first",2]`,
			question: `{"type":"score","name":"evaluation","instructions":"Assess the state.","levels":[{"label":"0","description":"Low"},{"label":"1","description":"Medium"},{"label":"2","description":"High"}]}`,
			answer:   `{"type":"score","name":"evaluation","score":2,"probabilities":[{"value":0,"label":"0","probability":0},{"value":1,"label":"1","probability":0},{"value":2,"label":"2","probability":1}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, got, _ := decisionsServer(t, http.StatusOK, decisionsBody(tc.answer))
			cfg := openaiConfig(tc.kind)
			cfg.BaseURL = server.URL + "/v1"
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			_, err = client.Evaluate(t.Context(), tc.state)
			require.NoError(t, err)
			assert.Equal(t, http.MethodPost, got.method)
			assert.Equal(t, "/v1/decisions", got.path)
			assert.Equal(t, "Bearer private-token", got.auth)
			assert.Equal(t, "gpt-6-luna", got.body["model"])
			assert.Equal(t, tc.input, got.body["input"])
			raw, err := json.Marshal(got.body["questions"])
			require.NoError(t, err)
			assert.JSONEq(t, "["+tc.question+"]", string(raw))
			assert.Len(t, got.body, 3, "no hidden context is sent")
		})
	}
}

func TestOpenAIEvaluateResults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   string
		body   string
		result string
	}{
		{
			name: "synthetic predicate fixture", kind: "boolean",
			body:   decisionsBody(`{"type":"predicate","name":"evaluation","probability":1.0}`, syntheticUsage),
			result: `{"type":"boolean","model":"gpt-6-luna","probability":1,"usage":{"input_tokens":160,"output_tokens":0}}`,
		},
		{
			name: "predicate zero with confidence", kind: "boolean",
			body:   decisionsBody(`{"type":"predicate","name":"evaluation","probability":0,"confidence":0}`),
			result: `{"type":"boolean","model":"gpt-6-luna","probability":0,"confidence":0,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "choice", kind: "choice",
			body:   decisionsBody(`{"type":"choice","name":"evaluation","choice":"unsafe","probabilities":[{"value":"safe","probability":0.25},{"value":"unsafe","probability":0.75}],"confidence":0.6}`),
			result: `{"type":"choice","model":"gpt-6-luna","choice":"unsafe","probabilities":{"safe":0.25,"unsafe":0.75},"confidence":0.6,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "tied choice", kind: "choice",
			body:   decisionsBody(`{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":0.5},{"value":"unsafe","probability":0.5}]}`),
			result: `{"type":"choice","model":"gpt-6-luna","choice":"safe","probabilities":{"safe":0.5,"unsafe":0.5},"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "score between levels", kind: "score",
			body:   decisionsBody(`{"type":"score","name":"evaluation","score":1.1,"probabilities":[{"value":0,"label":"0","probability":0.1},{"value":1,"label":"1","probability":0.7},{"value":2,"label":"2","probability":0.2}],"confidence":0.55}`),
			result: `{"type":"score","model":"gpt-6-luna","score":1.1,"probabilities":{"0":0.1,"1":0.7,"2":0.2},"confidence":0.55,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "score zero", kind: "score",
			body:   decisionsBody(`{"type":"score","name":"evaluation","score":0,"probabilities":[{"value":2,"probability":0},{"value":0,"probability":1},{"value":1,"probability":0}]}`),
			result: `{"type":"score","model":"gpt-6-luna","score":0,"probabilities":{"0":1,"1":0,"2":0},"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "model falls back to the requested one", kind: "boolean",
			body:   `{"answers":[` + predicateOne + `]}`,
			result: `{"type":"boolean","model":"gpt-6-luna","probability":1,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "returned model wins and unrelated metadata is allowed", kind: "boolean",
			body:   `{"id":"dec_1","model":"gpt-6-luna-2026-10-01","answers":[` + predicateOne + `]}`,
			result: `{"type":"boolean","model":"gpt-6-luna-2026-10-01","probability":1,"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _, _ := decisionsServer(t, http.StatusOK, tc.body)
			cfg := openaiConfig(tc.kind)
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), "state")
			require.NoError(t, err)
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			assert.JSONEq(t, tc.result, string(raw))
		})
	}
}

func TestOpenAIRejectsInvalidAnswers(t *testing.T) {
	t.Parallel()
	safe := `[{"value":"safe","probability":1},{"value":"unsafe","probability":0}]`
	levels := func(p0, p1, p2 string) string {
		return fmt.Sprintf(`[{"value":0,"probability":%s},{"value":1,"probability":%s},{"value":2,"probability":%s}]`, p0, p1, p2)
	}
	for _, tc := range []struct {
		name, kind, answers string
	}{
		{"no answers", "boolean", ``},
		{"other name", "boolean", `{"type":"predicate","name":"other","probability":1}`},
		{"duplicate names", "boolean", predicateOne + "," + predicateOne},
		{"refusal", "boolean", `{"type":"refusal","name":"evaluation"}`},
		{"unnamed refusal", "boolean", `{"type":"refusal"}`},
		{"refusal with another answer", "boolean", `{"type":"refusal","name":"evaluation"},` + predicateOne},
		{"type mismatch", "choice", predicateOne},
		{"unknown type", "boolean", `{"type":"other","name":"evaluation","probability":1}`},
		{"missing probability", "boolean", `{"type":"predicate","name":"evaluation"}`},
		{"null probability", "boolean", `{"type":"predicate","name":"evaluation","probability":null}`},
		{"string probability", "boolean", `{"type":"predicate","name":"evaluation","probability":"0.5"}`},
		{"large probability", "boolean", `{"type":"predicate","name":"evaluation","probability":1.1}`},
		{"negative probability", "boolean", `{"type":"predicate","name":"evaluation","probability":-0.1}`},
		{"invalid confidence", "boolean", `{"type":"predicate","name":"evaluation","probability":1,"confidence":2}`},
		{"missing choice", "choice", `{"type":"choice","name":"evaluation","probabilities":` + safe + `}`},
		{"unknown choice", "choice", `{"type":"choice","name":"evaluation","choice":"private-state","probabilities":` + safe + `}`},
		{"not highest", "choice", `{"type":"choice","name":"evaluation","choice":"unsafe","probabilities":[{"value":"safe","probability":0.8},{"value":"unsafe","probability":0.2}]}`},
		{"missing option", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":1}]}`},
		{"unknown option", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":1},{"value":"extra","probability":0}]}`},
		{"duplicate option", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":0.5},{"value":"safe","probability":0.5}]}`},
		{"numeric option", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":1},{"value":1,"probability":0}]}`},
		{"option without probability", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":1},{"value":"unsafe"}]}`},
		{"probabilities as object", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":{"safe":1,"unsafe":0}}`},
		{"bad sum", "choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":0.5},{"value":"unsafe","probability":0.1}]}`},
		{"missing score", "score", `{"type":"score","name":"evaluation","probabilities":` + levels("1", "0", "0") + `}`},
		{"null score", "score", `{"type":"score","name":"evaluation","score":null,"probabilities":` + levels("1", "0", "0") + `}`},
		{"string score", "score", `{"type":"score","name":"evaluation","score":"1","probabilities":` + levels("0", "1", "0") + `}`},
		{"score above range", "score", `{"type":"score","name":"evaluation","score":3,"probabilities":` + levels("0", "0", "1") + `}`},
		{"score below range", "score", `{"type":"score","name":"evaluation","score":-1,"probabilities":` + levels("1", "0", "0") + `}`},
		{"score contradicts probabilities", "score", `{"type":"score","name":"evaluation","score":2,"probabilities":` + levels("1", "0", "0") + `}`},
		{"string level index", "score", `{"type":"score","name":"evaluation","score":1,"probabilities":[{"value":"0","probability":0},{"value":"1","probability":1},{"value":"2","probability":0}]}`},
		{"fractional level index", "score", `{"type":"score","name":"evaluation","score":1,"probabilities":[{"value":0.5,"probability":0},{"value":1,"probability":1},{"value":2,"probability":0}]}`},
		{"out of range level index", "score", `{"type":"score","name":"evaluation","score":1,"probabilities":[{"value":1,"probability":0},{"value":2,"probability":1},{"value":3,"probability":0}]}`},
		{"duplicate level index", "score", `{"type":"score","name":"evaluation","score":0,"probabilities":[{"value":0,"probability":0.5},{"value":0,"probability":0.5},{"value":1,"probability":0}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _, requests := decisionsServer(t, http.StatusOK, decisionsBody(tc.answers, syntheticUsage))
			cfg := openaiConfig(tc.kind)
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			ctx, records := observe(t.Context())
			result, err := client.Evaluate(ctx, "private-state")
			require.Error(t, err)
			assert.Nil(t, result)
			assert.False(t, evaluator.IsTerminal(err), "answer failures allow routing fallback")
			assert.NotContains(t, err.Error(), "private-state")
			assert.NotContains(t, err.Error(), "private-token")
			assert.EqualValues(t, 1, requests.Load(), "no inference retry")
			require.Len(t, records(), 1, "invalid answers are still billable")
			assert.Equal(t, &evaluator.Usage{InputTokens: 160}, records()[0].Usage)
		})
	}
}

func TestOpenAIScoreConsistencyTolerance(t *testing.T) {
	t.Parallel()
	for score, valid := range map[string]bool{"1.1": true, "1.14": true, "1.06": true, "1.16": false, "1.04": false} {
		body := decisionsBody(`{"type":"score","name":"evaluation","score":` + score + `,"probabilities":[{"value":0,"probability":0.1},{"value":1,"probability":0.7},{"value":2,"probability":0.2}]}`)
		server, _, _ := decisionsServer(t, http.StatusOK, body)
		cfg := openaiConfig("score")
		cfg.BaseURL = server.URL
		client, err := New(t.Context(), cfg, openaiKey)
		require.NoError(t, err)
		_, err = client.Evaluate(t.Context(), "state")
		assert.Equal(t, valid, err == nil, score)
	}
}

func TestOpenAIUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		usage    string
		price    *latest.CostConfig
		terminal bool
		want     *evaluator.Usage
		cost     *float64
	}{
		{name: "hidden", want: nil},
		{name: "null", usage: `"usage":null`, want: nil},
		{name: "partial counts are unknown", usage: `"usage":{"input_tokens":10}`, want: nil},
		{name: "zero is reported", usage: `"usage":{"input_tokens":0,"output_tokens":0}`, want: &evaluator.Usage{}, price: &latest.CostConfig{}, cost: new(0.0)},
		{name: "unpriced", usage: `"usage":{"input_tokens":1000000,"output_tokens":0}`, want: &evaluator.Usage{InputTokens: 1000000}},
		{name: "override", usage: `"usage":{"input_tokens":1000000,"output_tokens":0}`, want: &evaluator.Usage{InputTokens: 1000000}, price: &latest.CostConfig{Input: 0.1}, cost: new(0.1)},
		{name: "negative", usage: `"usage":{"input_tokens":-1,"output_tokens":0}`, terminal: true},
		{name: "overflow", usage: `"usage":{"input_tokens":9223372036854775807,"output_tokens":1}`, terminal: true},
		{name: "wrong shape", usage: `"usage":"many"`, terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			extra := []string{}
			if tc.usage != "" {
				extra = append(extra, tc.usage)
			}
			server, _, _ := decisionsServer(t, http.StatusOK, decisionsBody(predicateOne, extra...))
			cfg := openaiConfig("boolean")
			cfg.BaseURL, cfg.Cost = server.URL, tc.price
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			ctx, records := observe(t.Context())
			result, err := client.Evaluate(ctx, "state")
			require.Len(t, records(), 1)
			if tc.terminal {
				require.Error(t, err)
				assert.True(t, evaluator.IsTerminal(err))
				assert.Nil(t, records()[0].Usage)
				return
			}
			require.NoError(t, err, "hidden usage never invalidates a valid assessment")
			assert.Equal(t, tc.want, records()[0].Usage)
			assert.Equal(t, tc.cost, records()[0].Cost)
			assert.Equal(t, tc.cost, result.Cost)
		})
	}
}

func TestOpenAIPriceOverrideIsCopied(t *testing.T) {
	t.Parallel()
	server, _, _ := decisionsServer(t, http.StatusOK, decisionsBody(predicateOne, syntheticUsage))
	cfg := openaiConfig("boolean")
	cfg.BaseURL, cfg.Cost = server.URL, &latest.CostConfig{Input: 1}
	client, err := New(t.Context(), cfg, openaiKey)
	require.NoError(t, err)
	cfg.Cost.Input = 1000
	result, err := client.Evaluate(t.Context(), "state")
	require.NoError(t, err)
	assert.InDelta(t, 160.0/1e6, *result.Cost, 1e-12)
}

func TestOpenAIHTTPFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"admission 400 json": {http.StatusBadRequest, `{"error":{"message":"definition too large private-state"}}`},
		"rate limited":       {http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`},
		"sanitized 500":      {http.StatusInternalServerError, ``},
		"sanitized 502 text": {http.StatusBadGateway, `upstream response replaced private-state`},
		"sanitized 502 json": {http.StatusBadGateway, `{"error":"bad gateway"}`},
		"unauthorized":       {http.StatusUnauthorized, `{"error":{"message":"bad key private-token"}}`},
		"no content":         {http.StatusNoContent, ``},
		"oversized error":    {http.StatusBadGateway, strings.Repeat("x", 2*maxResponseBytes)},
		"ok body on 500":     {http.StatusInternalServerError, decisionsBody(predicateOne, syntheticUsage)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, _, requests := decisionsServer(t, tc.status, tc.body)
			cfg := openaiConfig("boolean")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			ctx, records := observe(t.Context())
			result, err := client.Evaluate(ctx, "private-state")
			require.ErrorContains(t, err, fmt.Sprintf("HTTP status %d", tc.status))
			assert.Nil(t, result)
			assert.False(t, evaluator.IsTerminal(err))
			for _, secret := range []string{"private-state", "private-token", "slow down", "bad gateway"} {
				assert.NotContains(t, err.Error(), secret)
			}
			assert.EqualValues(t, 1, requests.Load(), "no inference retry")
			require.Len(t, records(), 1)
			assert.Equal(t, "gpt-6-luna", records()[0].Model)
			if name != "ok body on 500" {
				assert.Nil(t, records()[0].Usage, "unknown, not zero")
				assert.Nil(t, records()[0].Cost)
			}
		})
	}
}

func TestOpenAIResponseBound(t *testing.T) {
	t.Parallel()
	padded := func(size int) string {
		body := decisionsBody(predicateOne)
		return body + strings.Repeat(" ", size-len(body))
	}
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"exactly at the bound": {body: padded(maxResponseBytes)},
		"one byte over":        {body: padded(maxResponseBytes + 1), want: "exceeds size limit"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, _, _ := decisionsServer(t, http.StatusOK, tc.body)
			cfg := openaiConfig("boolean")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			ctx, records := observe(t.Context())
			_, err = client.Evaluate(ctx, "state")
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
			assert.Len(t, records(), 1)
		})
	}
}

func TestOpenAIUnreadableAndMalformedResponses(t *testing.T) {
	t.Parallel()
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"answers":[`)
	}))
	defer truncated.Close()
	cfg := openaiConfig("boolean")
	cfg.BaseURL = truncated.URL
	client, err := New(t.Context(), cfg, openaiKey)
	require.NoError(t, err)
	ctx, records := observe(t.Context())
	_, err = client.Evaluate(ctx, "state")
	require.ErrorContains(t, err, "failed to read evaluator response")
	assert.Len(t, records(), 1)

	for name, body := range map[string]string{
		"not json":           `nope`,
		"null":               `null`,
		"answers not array":  `{"answers":{"evaluation":{}}}`,
		"model not a string": `{"model":5,"answers":[` + predicateOne + `]}`,
		"trailing data":      decisionsBody(predicateOne) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, _, _ := decisionsServer(t, http.StatusOK, body)
			cfg := openaiConfig("boolean")
			cfg.BaseURL = server.URL
			client, err := New(t.Context(), cfg, openaiKey)
			require.NoError(t, err)
			ctx, records := observe(t.Context())
			result, err := client.Evaluate(ctx, "state")
			require.Error(t, err)
			assert.Nil(t, result)
			assert.False(t, evaluator.IsTerminal(err))
			assert.Len(t, records(), 1)
		})
	}
}

func TestOpenAIRefusesRedirects(t *testing.T) {
	t.Parallel()
	target, _, targetRequests := decisionsServer(t, http.StatusOK, decisionsBody(predicateOne))
	redirect := httptest.NewServer(http.RedirectHandler(target.URL+"/decisions", http.StatusTemporaryRedirect))
	defer redirect.Close()
	cfg := openaiConfig("boolean")
	cfg.BaseURL = redirect.URL
	client, err := New(t.Context(), cfg, openaiKey)
	require.NoError(t, err)
	_, err = client.Evaluate(t.Context(), "state")
	require.ErrorContains(t, err, "HTTP status 307")
	assert.Zero(t, targetRequests.Load(), "the credential is not replayed elsewhere")
}

func TestOpenAIMissingKeyAndInvalidState(t *testing.T) {
	t.Parallel()
	cfg := openaiConfig("boolean")
	client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	ctx := evaluator.WithUsageObserver(t.Context(), func(evaluator.UsageRecord) { t.Error("no request was attempted") })
	_, err = client.Evaluate(ctx, "state")
	require.ErrorContains(t, err, "API key is missing")

	client, err = New(t.Context(), cfg, environmentFunc(func(context.Context, string) (string, bool) {
		t.Error("invalid state looked up credentials")
		return "", false
	}))
	require.NoError(t, err)
	for _, state := range []any{nil, 1, true, make(chan int), map[string]string(nil)} {
		_, err = client.Evaluate(ctx, state)
		require.Error(t, err)
	}
}

func TestOpenAICustomTokenKeyIsReadPerEvaluation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, fmt.Sprintf("Bearer key-%d", calls.Load()), r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, decisionsBody(predicateOne))
	}))
	defer server.Close()
	cfg := openaiConfig("boolean")
	cfg.BaseURL, cfg.TokenKey = server.URL, "ROTATED"
	client, err := New(t.Context(), cfg, environmentFunc(func(_ context.Context, name string) (string, bool) {
		assert.Equal(t, "ROTATED", name)
		return fmt.Sprintf("key-%d", calls.Add(1)), true
	}))
	require.NoError(t, err)
	for range 2 {
		_, err = client.Evaluate(t.Context(), "state")
		require.NoError(t, err)
	}
}

func TestOpenAIDefinitionAdmission(t *testing.T) {
	t.Parallel()
	const budget = 512 << 10
	// The gateway estimates the response from the names and values it would echo.
	estimate := func(q map[string]any) int {
		size := len(q["name"].(string))
		for _, c := range q["choices"].([]any) {
			size += 2 * len(c.(map[string]any)["value"].(string))
		}
		return size
	}
	var admitted, rejected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions []map[string]any `json:"questions"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		size := 0
		for _, q := range req.Questions {
			switch {
			case q["choices"] != nil:
				size += estimate(q)
			case q["levels"] != nil:
				for _, l := range q["levels"].([]any) {
					size += 2 * len(l.(map[string]any)["label"].(string))
				}
			}
		}
		if size > budget {
			rejected.Add(1)
			http.Error(w, `{"error":"definition exceeds admission budget"}`, http.StatusBadRequest)
			return
		}
		admitted.Add(1)
		_, _ = io.WriteString(w, decisionsBody(predicateOne))
	}))
	defer server.Close()

	long := func(n int) map[string]string {
		choices := map[string]string{}
		for i := range 255 {
			choices[fmt.Sprintf("%0*d", n, i)] = "description that is never echoed " + strings.Repeat("d", 4096)
		}
		return choices
	}

	t.Run("long descriptions do not count", func(t *testing.T) {
		cfg := openaiConfig("choice")
		cfg.BaseURL, cfg.Choices = server.URL, long(8)
		client, err := New(t.Context(), cfg, openaiKey)
		require.NoError(t, err)
		before := admitted.Load()
		_, err = client.Evaluate(t.Context(), "state")
		require.ErrorContains(t, err, "does not match the question", "the canned answer is a predicate")
		assert.Equal(t, before+1, admitted.Load())
	})
	t.Run("long score labels stay short", func(t *testing.T) {
		cfg := openaiConfig("score")
		cfg.BaseURL, cfg.Levels = server.URL, []string{strings.Repeat("long level text ", 4096), "high"}
		client, err := New(t.Context(), cfg, openaiKey)
		require.NoError(t, err)
		before := admitted.Load()
		_, err = client.Evaluate(t.Context(), "state")
		require.ErrorContains(t, err, "does not match the question")
		assert.Equal(t, before+1, admitted.Load())
	})
	t.Run("oversized choice keys are rejected without altering the definition", func(t *testing.T) {
		cfg := openaiConfig("choice")
		cfg.BaseURL, cfg.Choices = server.URL, long(2048)
		before := rejected.Load()
		client, err := New(t.Context(), cfg, openaiKey)
		require.NoError(t, err)
		ctx, records := observe(t.Context())
		_, err = client.Evaluate(ctx, "state")
		require.ErrorContains(t, err, "HTTP status 400")
		assert.False(t, evaluator.IsTerminal(err))
		assert.NotContains(t, err.Error(), "admission")
		assert.Equal(t, before+1, rejected.Load(), "one attempt, no retry")
		require.Len(t, records(), 1)
		assert.Nil(t, records()[0].Usage)
	})
}
