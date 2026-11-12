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
	rc.ModelsGateway = "http://localhost:7777"
	flags, values = EnvForAgent(t.Context(), path, environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "chat-key", "CORPORATE_KEY": "evaluator-key",
	}), nil, rc)
	assert.Empty(t, flags)
	assert.Empty(t, values)
}
