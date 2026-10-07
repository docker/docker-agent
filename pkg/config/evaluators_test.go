package config

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestEvaluatorCredentialsFollowConnectionMode(t *testing.T) {
	t.Parallel()
	const gateway = "https://gateway.example.com"
	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{"corp": {Provider: "typesafe", TokenKey: "CORP_KEY"}},
		Evaluators: map[string]latest.EvaluatorConfig{
			"risk":   {Provider: "corp", Model: "jev-latest", Type: "boolean", Instructions: "Assess risk"},
			"unused": {Provider: "typesafe", Model: "jev-latest", Type: "boolean", Instructions: "Assess risk", TokenKey: "UNUSED_KEY"},
		},
		Agents: latest.Agents{{Name: "root", Model: "openai/gpt-5-mini"}},
	}
	assert.Empty(t, GatherEnvVarsForEvaluators(cfg))
	MergeAgentHooks(cfg, &latest.HooksConfig{ToolGuard: latest.HookMatcherConfigs{{Hooks: latest.HookDefinitions{{
		Type: "evaluator", Evaluator: "risk", EvaluatorPolicy: &latest.EvaluatorPolicy{
			Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "ask",
		},
	}}}}})
	require.NoError(t, cfg.Validate())
	assert.Equal(t, []string{"CORP_KEY"}, GatherEnvVarsForEvaluators(cfg))
	assert.Empty(t, GatherEnvVarsForEvaluatorsWithGateway(cfg, gateway), "the gateway authenticates on its own")

	err := CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}))
	require.ErrorContains(t, err, "CORP_KEY")
	require.NoError(t, CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test", "CORP_KEY": "test",
	})))
	require.NoError(t, CheckRequiredEnvVars(t.Context(), cfg, gateway, environment.NewMapEnvProvider(nil)))

	def := cfg.Evaluators["risk"]
	def.TokenKey = "OVERRIDE_KEY"
	cfg.Evaluators["risk"] = def
	assert.Equal(t, []string{"OVERRIDE_KEY"}, GatherEnvVarsForEvaluators(cfg))

	for _, direct := range []func(*latest.EvaluatorConfig){
		func(e *latest.EvaluatorConfig) { e.BypassModelsGateway = true },
		func(e *latest.EvaluatorConfig) { e.BaseURL = "https://laya.example.com" },
		func(e *latest.EvaluatorConfig) { e.Endpoint = "https://laya.example.com/predict" },
	} {
		def := cfg.Evaluators["risk"]
		direct(&def)
		cfg.Evaluators["risk"] = def
		assert.Equal(t, []string{"OVERRIDE_KEY"}, GatherEnvVarsForEvaluatorsWithGateway(cfg, gateway))
	}
}

func TestEvaluatorOpenAIEnvironmentDiscovery(t *testing.T) {
	t.Parallel()
	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{"decider": {Provider: "openai", Model: "gpt-6-luna", BaseURL: "${DECISION_BASE}"}},
		Evaluators: map[string]latest.EvaluatorConfig{
			"route":  {Model: "decider", Type: "boolean", Instructions: "Assess"},
			"inline": {Model: "openai/gpt-6-luna", Type: "boolean", Instructions: "Assess", Endpoint: "${DECISION_ENDPOINT}"},
			"plain":  {Model: "openai/gpt-6-luna", Type: "boolean", Instructions: "Assess"},
		},
		Agents: latest.Agents{{Name: "root", Model: "openai/gpt-5-mini"}},
	}
	hook := func(name string) latest.HookDefinition {
		return latest.HookDefinition{Type: "evaluator", Evaluator: name, EvaluatorPolicy: &latest.EvaluatorPolicy{
			Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "ask",
		}}
	}
	MergeAgentHooks(cfg, &latest.HooksConfig{ToolGuard: latest.HookMatcherConfigs{{Hooks: latest.HookDefinitions{hook("route"), hook("inline"), hook("plain")}}}})
	require.NoError(t, cfg.Validate())

	assert.Equal(t, []string{"DECISION_BASE", "DECISION_ENDPOINT", "OPENAI_API_KEY"}, GatherEnvVarsForEvaluators(cfg))
	assert.Equal(t, []string{"DECISION_BASE", "DECISION_ENDPOINT", "OPENAI_API_KEY"}, GatherEnvVarsForEvaluatorsWithGateway(cfg, "https://gateway.example.com"),
		"custom URLs stay direct, so the provider key is still needed")
}

func TestEvaluatorHookSchemaEventRestrictions(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(data))
	require.NoError(t, err)
	hook := map[string]any{
		"type": "evaluator", "evaluator": "risk",
		"evaluator_policy": map[string]any{"decisions": map[string]string{"true": "ask"}, "min_probability": 0.9, "fallback": "ask"},
	}
	for _, event := range []string{"tool_guard", "pre_tool_use", "permission_request", "session_start", "prompt_file_guard", "skill_content_guard"} {
		t.Run(event, func(t *testing.T) {
			t.Parallel()
			var hooks any = []any{map[string]any{"hooks": []any{hook}}}
			if event == "session_start" || event == "prompt_file_guard" || event == "skill_content_guard" {
				hooks = []any{hook}
			}
			cfg := map[string]any{"agents": map[string]any{"root": map[string]any{"model": "openai/gpt-5-mini", "hooks": map[string]any{event: hooks}}}}
			result, err := schema.Validate(gojsonschema.NewGoLoader(cfg))
			require.NoError(t, err)
			assert.Equal(t, event == "tool_guard", result.Valid(), "%v", result.Errors())
		})
	}
}

func TestEvaluatorSchemaRejectsInvalidDefinitions(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(data))
	require.NoError(t, err)
	for _, definition := range []string{
		`{"provider":"typesafe","model":"jev","type":"choice","instructions":"Assess"}`,
		`{"provider":"typesafe","model":"jev","type":"boolean","instructions":"Assess","choices":{"a":"","b":""}}`,
		`{"provider":"typesafe","model":"jev","type":"score","instructions":"Assess","levels":["Only"]}`,
	} {
		var def any
		require.NoError(t, json.Unmarshal([]byte(definition), &def))
		result, err := schema.Validate(gojsonschema.NewGoLoader(map[string]any{"evaluators": map[string]any{"test": def}}))
		require.NoError(t, err)
		assert.False(t, result.Valid(), definition)
	}
}
