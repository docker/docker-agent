package evaluation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
)

func TestCreateJudgeSelectsBackend(t *testing.T) {
	t.Parallel()

	runConfig := &config.RuntimeConfig{
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}),
		ProviderRegistry:    providers.NewDefaultRegistry(),
	}
	for _, tt := range []struct {
		name, judgeType, model, wantErr string
		evaluator, empty                bool
	}{
		{"default LLM", "", "openai/gpt-5", "", false, false},
		{"explicit LLM", "llm", "openai/gpt-5", "", false, false},
		{"evaluator", "evaluator", "typesafe/jev-latest", "", true, false},
		{"empty LLM", "llm", "", "", false, true},
		{"empty evaluator", "evaluator", "", "", false, true},
		{"unknown type", "typo", "typesafe/jev-latest", "invalid judge type", false, false},
		{"bad LLM ref", "llm", "invalid", "invalid judge model format", false, false},
		{"bad evaluator ref", "evaluator", "missing", "expected 'provider/model' or a named evaluator", false, false},
		{"openai evaluator", "evaluator", "openai/gpt-6-luna", "", true, false},
		{"unsupported evaluator", "evaluator", "anthropic/claude", "unsupported evaluator provider", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			judge, err := createJudge(t.Context(), Config{JudgeType: tt.judgeType, JudgeModel: tt.model, Concurrency: 3}, runConfig, nil)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.empty {
				assert.Nil(t, judge)
				return
			}
			require.NotNil(t, judge)
			assert.Equal(t, 3, judge.concurrency)
			if tt.evaluator {
				assert.IsType(t, &evaluatorJudge{}, judge.backend)
			} else {
				assert.IsType(t, &llmJudge{}, judge.backend)
			}
		})
	}
}

func TestCreateEvaluatorJudgeConfiguration(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"agent provider", "global provider", "named evaluator"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			instructions := relevanceEvaluatorInstructions
			path := "/v1/systemone"
			if mode == "named evaluator" {
				instructions = "Does transcript satisfy criterion?"
				path = "/development/predict"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, path, r.URL.Path)
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				var payload struct {
					Model     string            `json:"model"`
					State     map[string]string `json:"state"`
					Questions map[string]struct {
						Type         string `json:"type"`
						Instructions string `json:"instructions"`
					} `json:"questions"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				assert.Equal(t, "english", payload.Model)
				assert.Equal(t, map[string]string{"transcript": "The sky is blue.", "criterion": "The response mentions a color."}, payload.State)
				assert.Equal(t, "noul", payload.Questions["evaluation"].Type)
				assert.Equal(t, instructions, payload.Questions["evaluation"].Instructions)
				_, err := io.WriteString(w, `{"model":"english","answers":{"evaluation":{"type":"noul","noul":0.9}}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			connection := latest.ProviderConfig{Provider: "typesafe", BaseURL: server.URL, TokenKey: "JUDGE_KEY"}
			runConfig := &config.RuntimeConfig{
				Config:              config.Config{Providers: map[string]latest.ProviderConfig{"assessments": connection}},
				EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"JUDGE_KEY": "secret"}),
			}
			agentConfig := &latest.Config{}
			model := "assessments/english"
			if mode != "global provider" {
				agentConfig.Providers = map[string]latest.ProviderConfig{"assessments": connection}
				runConfig.Providers["assessments"] = latest.ProviderConfig{Provider: "typesafe", BaseURL: "http://wrong.invalid", TokenKey: "WRONG"}
			}
			if mode == "named evaluator" {
				model = "relevance"
				agentConfig.Evaluators = map[string]latest.EvaluatorConfig{"relevance": {
					Provider: "assessments", Model: "english", Type: "boolean", Instructions: instructions,
					Endpoint: server.URL + path, Cost: &latest.CostConfig{},
				}}
			}
			judge, err := createJudge(t.Context(), Config{JudgeType: JudgeTypeEvaluator, JudgeModel: model}, runConfig, agentConfig)
			require.NoError(t, err)
			require.NoError(t, judge.Validate(t.Context()))
			if mode == "global provider" {
				assert.Nil(t, agentConfig.Providers, "factory must not mutate agent config")
			}
		})
	}
}

func TestCreateJudgeRejectsNonBooleanEvaluator(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"choice", "score"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			_, err := createJudge(t.Context(), Config{JudgeType: JudgeTypeEvaluator, JudgeModel: "relevance"},
				&config.RuntimeConfig{}, &latest.Config{Evaluators: map[string]latest.EvaluatorConfig{
					"relevance": {Type: kind},
				}})
			require.ErrorContains(t, err, "must use type boolean")
		})
	}
}

func TestEvaluatorJudgeGateway(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"typesafe", "openai"} {
		for _, named := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/named=%t", backend, named), func(t *testing.T) {
				t.Parallel()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "Bearer docker-token", r.Header.Get("Authorization"))
					var payload map[string]json.RawMessage
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					assert.JSONEq(t, `"encrypted"`, string(payload["encrypted_agent_config"]))
					response := `{"model":"jev-latest","answers":{"evaluation":{"type":"noul","noul":1}},"usage":{"input_tokens":12,"output_tokens":0}}`
					if backend == "openai" {
						assert.Equal(t, "/v1/decisions", r.URL.Path)
						response = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"evaluation","probability":1}],"usage":{"input_tokens":12,"output_tokens":0}}`
						var state string
						assert.NoError(t, json.Unmarshal(payload["input"], &state))
						assert.Contains(t, state, `"criterion":`)
					} else {
						assert.Equal(t, "/v1/systemone", r.URL.Path)
					}
					_, err := io.WriteString(w, response)
					assert.NoError(t, err)
				}))
				t.Cleanup(server.Close)
				model := "jev-latest"
				if backend == "openai" {
					model = "gpt-6-luna"
				}
				ref := backend + "/" + model
				agentConfig := &latest.Config{}
				if named {
					ref = "relevance"
					agentConfig.Providers = map[string]latest.ProviderConfig{"corp": {Provider: backend, TokenKey: "UNNEEDED_KEY"}}
					agentConfig.Evaluators = map[string]latest.EvaluatorConfig{ref: {
						Provider: "corp", Model: model, Type: "boolean", Instructions: relevanceEvaluatorInstructions,
					}}
				}
				rc := &config.RuntimeConfig{
					Config:              config.Config{ModelsGateway: server.URL, EncryptedConfig: "encrypted"},
					EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{environment.DockerDesktopTokenEnv: "docker-token"}),
				}
				judge, err := createJudge(t.Context(), Config{JudgeType: JudgeTypeEvaluator, JudgeModel: ref}, rc, agentConfig)
				require.NoError(t, err)
				require.NoError(t, judge.Validate(t.Context()))
			})
		}
	}
}
