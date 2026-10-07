package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestEnvForAgentEvaluatorProviderDefaults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
evaluators:
  risk:
    provider: corporate
    model: jev-latest
    type: boolean
    instructions: Assess risk.
agents:
  root:
    model: openai/gpt-5-mini
`), 0o600))
	rc := &config.RuntimeConfig{}
	rc.Providers = map[string]latest.ProviderConfig{"corporate": {Provider: "typesafe", TokenKey: "CORPORATE_KEY"}}
	rc.GlobalHooks = &latest.HooksConfig{ToolGuard: latest.HookMatcherConfigs{{Hooks: latest.HookDefinitions{{
		Type: "evaluator", Evaluator: "risk", EvaluatorPolicy: &latest.EvaluatorPolicy{
			Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "ask",
		},
	}}}}}
	flags, values := EnvForAgent(t.Context(), path, environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "chat-key", "CORPORATE_KEY": "evaluator-key",
	}), nil, rc)
	assert.Equal(t, []string{"-e", "CORPORATE_KEY"}, flags)
	assert.Equal(t, []string{"CORPORATE_KEY=evaluator-key"}, values)
}

func TestEnvForAgentOpenAIEvaluatorFollowsGateway(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
evaluators:
  route:
    model: openai/gpt-6-luna
    token_key: DECISION_KEY
    type: boolean
    instructions: Assess.
  direct:
    model: openai/gpt-6-luna
    token_key: DIRECT_KEY
    bypass_models_gateway: true
    type: boolean
    instructions: Assess.
agents:
  root:
    model: dmr/ai/qwen3
`), 0o600))
	hook := func(name string) latest.HookDefinition {
		return latest.HookDefinition{Type: "evaluator", Evaluator: name, EvaluatorPolicy: &latest.EvaluatorPolicy{
			Decisions: map[string]string{"true": "ask"}, MinProbability: 0.9, Fallback: "ask",
		}}
	}
	env := environment.NewMapEnvProvider(map[string]string{"DECISION_KEY": "a", "DIRECT_KEY": "b"})
	for gateway, want := range map[string][]string{
		"":                            {"DECISION_KEY", "DIRECT_KEY"},
		"https://gateway.example.com": {"DIRECT_KEY"},
	} {
		rc := &config.RuntimeConfig{}
		rc.ModelsGateway = gateway
		rc.GlobalHooks = &latest.HooksConfig{ToolGuard: latest.HookMatcherConfigs{{Hooks: latest.HookDefinitions{hook("route"), hook("direct")}}}}
		flags, _ := EnvForAgent(t.Context(), path, env, nil, rc)
		var names []string
		for i := 1; i < len(flags); i += 2 {
			names = append(names, flags[i])
		}
		assert.Equal(t, want, names, "gateway %q", gateway)
	}
}
