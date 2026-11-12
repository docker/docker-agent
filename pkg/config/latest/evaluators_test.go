package latest

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluatorConfigValidation(t *testing.T) {
	t.Parallel()
	base := EvaluatorConfig{Provider: "typesafe", Model: "jev-latest", Type: "boolean", Instructions: "Does this expose credentials?"}
	for _, tc := range []struct {
		name string
		edit func(*EvaluatorConfig)
		want string
	}{
		{"boolean", func(*EvaluatorConfig) {}, ""},
		{"choice", func(e *EvaluatorConfig) {
			e.Type = "choice"
			e.Choices = map[string]string{"simple": "Routine", "complex": "Reasoning"}
		}, ""},
		{"score", func(e *EvaluatorConfig) { e.Type = "score"; e.Levels = []string{"Low", "High"} }, ""},
		{"provider", func(e *EvaluatorConfig) { e.Provider = " " }, "provider and model"},
		{"model", func(e *EvaluatorConfig) { e.Model = "" }, "provider and model"},
		{"instructions", func(e *EvaluatorConfig) { e.Instructions = " " }, "instructions"},
		{"type", func(e *EvaluatorConfig) { e.Type = "noul" }, "unsupported evaluator type"},
		{"timeout", func(e *EvaluatorConfig) { e.Timeout.Duration = -time.Second }, "timeout"},
		{"base url", func(e *EvaluatorConfig) { e.BaseURL = "https://user:secret@example.com" }, "base_url"},
		{"url query", func(e *EvaluatorConfig) { e.BaseURL = "https://example.com?token=secret" }, "base_url"},
		{"boolean criteria", func(e *EvaluatorConfig) { e.Levels = []string{"yes"} }, "cannot define"},
		{"missing choices", func(e *EvaluatorConfig) { e.Type = "choice" }, "2-255"},
		{"empty choice", func(e *EvaluatorConfig) { e.Type = "choice"; e.Choices = map[string]string{" ": "", "other": ""} }, "choice names"},
		{"too many choices", func(e *EvaluatorConfig) {
			e.Type = "choice"
			e.Choices = map[string]string{}
			for i := range 256 {
				e.Choices[strings.Repeat("x", i+1)] = ""
			}
		}, "2-255"},
		{"missing levels", func(e *EvaluatorConfig) { e.Type = "score" }, "2-10"},
		{"empty level", func(e *EvaluatorConfig) { e.Type = "score"; e.Levels = []string{"low", " "} }, "levels must not"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := base
			tc.edit(&e)
			err := e.Validate()
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestEvaluatorResolve(t *testing.T) {
	t.Parallel()
	cfg := EvaluatorConfig{Provider: "corp", Model: "jev-latest", Type: "boolean", Instructions: "Assess risk"}
	providers := map[string]ProviderConfig{"corp": {Provider: "typesafe", BaseURL: "https://example.com", TokenKey: "CORP_KEY"}}
	resolved, err := cfg.Resolve(providers)
	require.NoError(t, err)
	assert.Equal(t, "typesafe", resolved.Provider)
	assert.Equal(t, "CORP_KEY", resolved.TokenKey)
	assert.Equal(t, "https://example.com", resolved.BaseURL)
	assert.Equal(t, "corp", cfg.Provider)
	assert.Empty(t, cfg.BaseURL)
	cfg.BaseURL, cfg.TokenKey = "https://override.example.com", "OVERRIDE_KEY"
	resolved, err = cfg.Resolve(providers)
	require.NoError(t, err)
	assert.Equal(t, cfg.BaseURL, resolved.BaseURL)
	assert.Equal(t, cfg.TokenKey, resolved.TokenKey)
	_, err = cfg.Resolve(nil)
	require.ErrorContains(t, err, "unsupported evaluator provider")
	providers["corp"] = ProviderConfig{Provider: "typesafe", APIType: "openai_responses"}
	_, err = cfg.Resolve(providers)
	require.ErrorContains(t, err, "do not support")
	cfg.Provider, cfg.TokenKey = "typesafe", ""
	resolved, err = cfg.Resolve(nil)
	require.NoError(t, err)
	assert.Equal(t, "TYPESAFE_API_KEY", resolved.TokenKey)
	cfg.Provider = "openai"
	resolved, err = cfg.Resolve(nil)
	require.NoError(t, err)
	assert.Equal(t, "OPENAI_API_KEY", resolved.TokenKey)
	cfg.Provider = "corporate"
	resolved, err = cfg.Resolve(map[string]ProviderConfig{"corporate": {Provider: "openai", TokenKey: "CORP_OPENAI_KEY"}})
	require.NoError(t, err)
	assert.Equal(t, "openai", resolved.Provider)
	assert.Equal(t, "CORP_OPENAI_KEY", resolved.TokenKey)
}

func TestEvaluatorPolicyValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy *EvaluatorPolicy
		want   string
	}{
		{"missing", nil, "required"},
		{"empty decisions", &EvaluatorPolicy{}, "decisions"},
		{"missing probability", &EvaluatorPolicy{Decisions: map[string]string{"true": "ask"}, Fallback: "ask"}, "min_probability"},
		{"nan", &EvaluatorPolicy{Decisions: map[string]string{"true": "ask"}, MinProbability: math.NaN(), Fallback: "ask"}, "min_probability"},
		{"infinite", &EvaluatorPolicy{Decisions: map[string]string{"true": "ask"}, MinProbability: math.Inf(1), Fallback: "ask"}, "min_probability"},
		{"allow fallback", &EvaluatorPolicy{Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "allow"}, "fallback"},
		{"invalid decision", &EvaluatorPolicy{Decisions: map[string]string{"true": "yes"}, MinProbability: 0.9, Fallback: "ask"}, "decisions"},
		{"valid", &EvaluatorPolicy{Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "deny"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.policy.Validate()
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestEvaluatorConfigRoundTripAndReferences(t *testing.T) {
	t.Parallel()
	const src = `evaluators:
  risk:
    provider: typesafe
    model: jev-latest
    endpoint: https://example.com/development/predict
    type: boolean
    instructions: Does this expose credentials?
    timeout: 2s
    cost:
      input: 0.042
      output: 0
agents:
  root:
    model: openai/gpt-5-mini
    hooks:
      tool_guard:
        - hooks:
            - type: evaluator
              evaluator: risk
              evaluator_policy:
                decisions: {"true": ask, "false": allow}
                min_probability: 0.95
                fallback: ask
`
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))
	assert.Equal(t, "https://example.com/development/predict", cfg.Evaluators["risk"].Endpoint)
	assert.Equal(t, 2*time.Second, cfg.Evaluators["risk"].Timeout.Duration)
	assert.Equal(t, &CostConfig{Input: 0.042}, cfg.Evaluators["risk"].Cost)
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	var decoded Config
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NoError(t, decoded.Validate())
	assert.Equal(t, cfg.Evaluators, decoded.Evaluators)

	for _, tc := range []struct{ name, old, replacement, want string }{
		{"unknown reference", "evaluator: risk", "evaluator: missing", "unknown evaluator"},
		{"wrong event", "tool_guard:", "pre_tool_use:", "only supported on tool_guard"},
		{"wrong boolean key", `"true": ask`, `"yes": ask`, "unknown boolean outcome"},
		{"wrong hook type", "type: evaluator", "type: command\n              command: echo", "evaluator fields require"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var invalid Config
			err := yaml.Unmarshal([]byte(strings.Replace(src, tc.old, tc.replacement, 1)), &invalid)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestEvaluatorCostValidation(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]float64{
		"negative": -1, "NaN": math.NaN(), "positive infinity": math.Inf(1), "negative infinity": math.Inf(-1),
	} {
		for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
			t.Run(name+"/"+field, func(t *testing.T) {
				t.Parallel()
				cfg := EvaluatorConfig{Provider: "typesafe", Model: "jev-latest", Type: "boolean", Instructions: "Assess risk", Cost: &CostConfig{}}
				switch field {
				case "input":
					cfg.Cost.Input = value
				case "output":
					cfg.Cost.Output = value
				case "cache_read":
					cfg.Cost.CacheRead = value
				case "cache_write":
					cfg.Cost.CacheWrite = value
				}
				require.ErrorContains(t, cfg.Validate(), "cost")
				_, err := cfg.Resolve(nil)
				require.ErrorContains(t, err, "cost")
			})
		}
	}
	for _, cost := range []*CostConfig{nil, {}, {Input: 0.042, Output: 1}} {
		cfg := EvaluatorConfig{Provider: "typesafe", Model: "jev-latest", Type: "boolean", Instructions: "Assess risk", Cost: cost}
		require.NoError(t, cfg.Validate())
		data, err := json.Marshal(cfg)
		require.NoError(t, err)
		var decoded EvaluatorConfig
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Equal(t, cost, decoded.Cost)
	}
}

func TestEvaluatorURLValidation(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"base_url", "endpoint"} {
		for _, tc := range []struct {
			name  string
			value string
			valid bool
		}{
			{"empty", "", true},
			{"https", "https://example.com/development/predict", true},
			{"http", "http://localhost:8000/predict/", true},
			{"relative", "/development/predict", false},
			{"scheme", "ftp://example.com/predict", false},
			{"missing host", "https://", false},
			{"credentials", "https://user:private-token@example.com/predict", false},
			{"query", "https://example.com/predict?token=private-token", false},
			{"empty query", "https://example.com/predict?", false},
			{"fragment", "https://example.com/predict#private-token", false},
			{"whitespace", " ", false},
			{"invalid escape", "https://example.com/%private-token", false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := EvaluatorConfig{Provider: "typesafe", Model: "english", Type: "boolean", Instructions: "Assess risk."}
				if field == "base_url" {
					cfg.BaseURL = tc.value
				} else {
					cfg.Endpoint = tc.value
				}
				err := cfg.Validate()
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, field)
					assert.NotContains(t, err.Error(), "private-token")
				}
			})
		}
	}
}

func TestEvaluatorResolveEndpoint(t *testing.T) {
	t.Parallel()

	cfg := EvaluatorConfig{
		Provider: "laya", Model: "english", Type: "boolean", Instructions: "Assess risk.",
		Endpoint: "https://example.com/development/predict",
	}
	providers := map[string]ProviderConfig{
		"laya": {Provider: "typesafe", BaseURL: "https://other.example.com", TokenKey: "BASETEN_API_KEY"},
	}
	resolved, err := cfg.Resolve(providers)
	require.NoError(t, err)
	assert.Equal(t, cfg.Endpoint, resolved.Endpoint)
	assert.Equal(t, providers["laya"].BaseURL, resolved.BaseURL)
	assert.Equal(t, "BASETEN_API_KEY", resolved.TokenKey)
	assert.Equal(t, "typesafe", resolved.Provider)
	assert.Equal(t, "laya", cfg.Provider)
	assert.Empty(t, cfg.BaseURL)
}
