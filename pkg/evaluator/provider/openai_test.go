package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

func openAIResponse(answer string) string {
	return `{"model":"gpt-6-luna","answers":[` + answer + `],"usage":{"input_tokens":12,"output_tokens":0,"total_tokens":12}}`
}

func TestOpenAIPrimitives(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind, question, answer, want string
		state                        any
	}{
		{
			"boolean", `{"type":"predicate","name":"evaluation","instructions":"Assess the state."}`,
			`{"type":"predicate","name":"evaluation","probability":0}`,
			`{"type":"boolean","model":"gpt-6-luna","probability":0,"usage":{"input_tokens":12,"output_tokens":0}}`, "text\nwith quotes \"",
		},
		{
			"choice", `{"type":"choice","name":"evaluation","instructions":"Assess the state.","choices":[{"value":"safe","description":"Safe to proceed"},{"value":"unsafe","description":"Do not proceed"}]}`,
			`{"type":"choice","name":"evaluation","choice":"safe","confidence":0,"probabilities":[{"value":"unsafe","probability":0},{"value":"safe","probability":1}]}`,
			`{"type":"choice","model":"gpt-6-luna","choice":"safe","confidence":0,"probabilities":{"safe":1,"unsafe":0},"usage":{"input_tokens":12,"output_tokens":0}}`,
			map[string]any{"tool_name": "shell", "tool_input": map[string]string{"cmd": "ls"}},
		},
		{
			"score", `{"type":"score","name":"evaluation","instructions":"Assess the state.","levels":[{"label":"0","description":"Low"},{"label":"1","description":"Medium"},{"label":"2","description":"High"}]}`,
			`{"type":"score","name":"evaluation","score":1.1,"confidence":0.8,"probabilities":[{"value":0,"probability":0.1},{"value":1,"probability":0.7},{"value":2,"probability":0.2}]}`,
			`{"type":"score","model":"gpt-6-luna","score":1.1,"confidence":0.8,"probabilities":{"0":0.1,"1":0.7,"2":0.2},"usage":{"input_tokens":12,"output_tokens":0}}`,
			[]any{"one", map[string]string{"message": "two"}},
		},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/decisions", r.URL.Path)
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				var payload struct {
					Model     string            `json:"model"`
					Input     string            `json:"input"`
					Questions []json.RawMessage `json:"questions"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				assert.Equal(t, "gpt-6-luna", payload.Model)
				if input, ok := tt.state.(string); ok {
					assert.Equal(t, input, payload.Input)
				} else {
					encoded, err := json.Marshal(tt.state)
					assert.NoError(t, err)
					assert.JSONEq(t, string(encoded), payload.Input)
				}
				if assert.Len(t, payload.Questions, 1) {
					assert.JSONEq(t, tt.question, string(payload.Questions[0]))
				}
				_, err := io.WriteString(w, openAIResponse(tt.answer))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			cfg := testConfig(tt.kind)
			cfg.Provider, cfg.Model, cfg.BaseURL = "openai", "gpt-6-luna", server.URL+"/v1/"
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret"}))
			require.NoError(t, err)
			result, err := client.Evaluate(t.Context(), tt.state)
			require.NoError(t, err)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(encoded))
		})
	}
}

func TestOpenAIRejectsInvalidAnswers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ kind, answer string }{
		{"boolean", `null`},
		{"boolean", `{"type":"refusal","name":"evaluation"}`},
		{"boolean", `{"type":"predicate","name":"other","probability":1}`},
		{"boolean", `{"type":"predicate","name":"evaluation","probability":null}`},
		{"boolean", `{"type":"predicate","name":"evaluation","probability":1.1}`},
		{"boolean", `{"type":"noul","name":"evaluation","probability":1}`},
		{"boolean", `{"type":"predicate","name":"evaluation","probability":1,"confidence":-1}`},
		{"boolean", `{"type":"predicate","name":"evaluation","probability":1},{"type":"predicate","name":"evaluation","probability":0}`},
		{"choice", `{"type":"choice","name":"evaluation","choice":true,"probabilities":[]}`},
		{"choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":1},{"value":"unsafe","probability":0},{"value":"safe","probability":0}]}`},
		{"choice", `{"type":"choice","name":"evaluation","choice":"unsafe","probabilities":[{"value":"safe","probability":1},{"value":"unsafe","probability":0}]}`},
		{"choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":"safe","probability":0.1},{"value":"unsafe","probability":0.1}]}`},
		{"choice", `{"type":"choice","name":"evaluation","choice":"safe","probabilities":[{"value":null,"probability":1},{"value":"unsafe","probability":0}]}`},
		{"score", `{"type":"score","name":"evaluation","score":0,"probabilities":[{"value":"0","probability":1}]}`},
		{"score", `{"type":"score","name":"evaluation","score":0,"probabilities":[{"value":null,"probability":1}]}`},
		{"score", `{"type":"score","name":"evaluation","score":0,"probabilities":[{"value":0.5,"probability":1}]}`},
		{"score", `{"type":"score","name":"evaluation","score":3,"probabilities":[{"value":0,"probability":1},{"value":1,"probability":0},{"value":2,"probability":0}]}`},
	} {
		t.Run(tt.kind+tt.answer, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, openAIResponse(tt.answer))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			cfg := testConfig(tt.kind)
			cfg.Provider, cfg.BaseURL = "openai", server.URL
			client, err := New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "private-token"}))
			require.NoError(t, err)
			var records []evaluator.UsageRecord
			ctx := evaluator.WithUsageObserver(t.Context(), func(record evaluator.UsageRecord) { records = append(records, record) })
			result, err := client.Evaluate(ctx, "private-state")
			require.Error(t, err)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), "private-state")
			assert.NotContains(t, err.Error(), "private-token")
			require.Len(t, records, 1)
			assert.Equal(t, &evaluator.Usage{InputTokens: 12}, records[0].Usage)
		})
	}
}

func TestOpenAIEndpointAndPricing(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"", openAIBaseURL + "/decisions", "https://custom.example/v1/decisions"} {
		cfg := testConfig("boolean")
		cfg.Provider, cfg.Model, cfg.Endpoint = "openai", "gpt-6-luna", endpoint
		client, err := New(t.Context(), cfg, environment.NewNoEnvProvider())
		require.NoError(t, err)
		p := client.(*openAI)
		if endpoint == "" {
			assert.Equal(t, openAIBaseURL+"/decisions", p.endpoint)
			assert.Equal(t, "OPENAI_API_KEY", p.tokenKey)
		} else {
			assert.Equal(t, endpoint, p.endpoint)
		}
		cost := p.estimateCost("gpt-6-luna", &evaluator.Usage{InputTokens: 1_000_000})
		if strings.HasPrefix(endpoint, "https://custom") {
			assert.Nil(t, cost)
		} else {
			require.NotNil(t, cost)
			assert.InDelta(t, 0.1, *cost, 1e-9)
		}
		assert.Nil(t, p.estimateCost("future", &evaluator.Usage{InputTokens: 1}))
	}
}
