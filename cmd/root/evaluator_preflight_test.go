package root

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

const evaluatorPreflightAgent = `
evaluators:
  risk:
    provider: typesafe
    model: jev-latest
    token_key: EVALUATOR_PREFLIGHT_KEY
    type: boolean
    instructions: Assess risk.
agents:
  root:
    model: openai/gpt-5-mini
`

const evaluatorPreflightHook = `tool_guard:
  - hooks:
      - type: evaluator
        evaluator: risk
        evaluator_policy:
          decisions: {"true": ask}
          min_probability: 0.9
          fallback: ask
`

func writeEvaluatorPreflightFixture(t *testing.T, hooksInSettings bool) (workspace, agentPath string) {
	t.Helper()

	configDir := t.TempDir()
	paths.SetConfigDir(configDir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	userConfig := "{}\n"
	if hooksInSettings {
		userConfig = "settings:\n  hooks:\n    " + strings.ReplaceAll(strings.TrimSpace(evaluatorPreflightHook), "\n", "\n    ") + "\n"
	} else {
		dropInDir := filepath.Join(configDir, "hooks.d")
		require.NoError(t, os.MkdirAll(dropInDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dropInDir, "risk.yaml"), []byte(evaluatorPreflightHook), 0o600))
	}
	require.NoError(t, os.WriteFile(userconfig.Path(), []byte(userConfig), 0o600))

	workspace = t.TempDir()
	agentPath = filepath.Join(workspace, "agent with spaces.yaml")
	require.NoError(t, os.WriteFile(agentPath, []byte(evaluatorPreflightAgent), 0o600))
	return workspace, agentPath
}

func TestDoctorDiscoversDropInEvaluatorRequirements(t *testing.T) {
	_, agentPath := writeEvaluatorPreflightFixture(t, false)

	for _, tc := range []struct {
		name   string
		secret string
	}{
		{name: "missing credential"},
		{name: "credential found", secret: "test-doctor-evaluator-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := executeDoctor(t, []string{"--json", agentPath},
				withDoctorTestEnv(map[string]string{
					"OPENAI_API_KEY":          "test-model-secret",
					"EVALUATOR_PREFLIGHT_KEY": tc.secret,
				}, nil, nil),
				func(f *doctorFlags) {
					f.loadUserConfig = userconfig.Load
					f.loadHookDropIns = config.LoadHookDropIns
				})
			found := tc.secret != ""
			if found {
				require.NoError(t, err)
				assert.NotContains(t, output, tc.secret)
			} else {
				var statusErr cli.StatusError
				require.ErrorAs(t, err, &statusErr)
				assert.Equal(t, 1, statusErr.StatusCode)
			}

			var report doctorReport
			require.NoError(t, json.Unmarshal([]byte(output), &report))
			require.NotNil(t, report.AgentFile)
			source := ""
			if found {
				source = "environment"
			}
			assert.ElementsMatch(t, []doctorEnvRequirement{
				{EnvVar: "OPENAI_API_KEY", RequiredBy: "models", Found: true, Source: "environment"},
				{EnvVar: "EVALUATOR_PREFLIGHT_KEY", RequiredBy: "evaluators", Found: found, Source: source},
			}, report.AgentFile.Requirements)
			if found {
				assert.Empty(t, report.Issues)
			} else {
				require.Len(t, report.Issues, 1)
				assert.Contains(t, report.Issues[0], "requires environment variables that are not set: EVALUATOR_PREFLIGHT_KEY")
			}
			assert.NotContains(t, output, "test-model-secret")
		})
	}
}
