package root

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/docker/cli/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
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

func TestRunInSandboxForwardsInheritedEvaluatorCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Docker script is POSIX-only")
	}

	for _, tc := range []struct {
		name            string
		hooksInSettings bool
	}{
		{name: "settings only", hooksInSettings: true},
		{name: "hooks.d only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, agentPath := writeEvaluatorPreflightFixture(t, tc.hooksInSettings)
			t.Setenv("SANDBOX_VM_ID", "")

			// Use the config env file so process inheritance cannot mask missing forwarding.
			const key = "EVALUATOR_PREFLIGHT_KEY"
			const secret = "test-evaluator-secret"
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
			require.NoError(t, os.WriteFile(filepath.Join(paths.GetConfigDir(), ".env"), []byte(key+"="+secret+"\n"), 0o600))

			fakeDir := t.TempDir()
			argsFile := filepath.Join(fakeDir, "exec.args")
			envFile := filepath.Join(fakeDir, "exec.env")
			listing, err := json.Marshal(map[string]any{
				"sandboxes": []map[string]any{{
					"name":       "evaluator-test",
					"workspaces": []string{workspace, paths.GetConfigDir() + ":ro"},
				}},
			})
			require.NoError(t, err)
			t.Setenv("EVALUATOR_TEST_SANDBOX_LIST", string(listing))
			t.Setenv("EVALUATOR_TEST_EXEC_ARGS", argsFile)
			t.Setenv("EVALUATOR_TEST_EXEC_ENV", envFile)
			t.Setenv("PATH", fakeDir)
			require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "docker"), []byte(`#!/bin/sh
set -eu
case "$1 $2" in
  "sandbox version"|"sandbox policy"|"sandbox create") ;;
  "sandbox ls") printf '%s\n' "$EVALUATOR_TEST_SANDBOX_LIST" ;;
  "sandbox exec")
    printf '%s\000' "$@" > "$EVALUATOR_TEST_EXEC_ARGS"
    printf '%s' "${EVALUATOR_PREFLIGHT_KEY-}" > "$EVALUATOR_TEST_EXEC_ENV"
    ;;
  *) printf 'unexpected Docker invocation: %s\n' "$*" >&2; exit 99 ;;
esac
`), 0o700))

			rc := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}
			require.Nil(t, rc.GlobalHooks, "sandbox dispatch happens before runOrExec loads hooks")
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			require.NoError(t, runInSandbox(t.Context(), cmd, []string{agentPath}, rc, false, true, sandbox.Options{}))
			assert.Nil(t, rc.GlobalHooks, "preflight must not mutate the caller's runtime config")

			argsData, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			argv := strings.Split(strings.TrimSuffix(string(argsData), "\x00"), "\x00")
			keyIndex := slices.Index(argv, key)
			require.Positive(t, keyIndex, "evaluator credential must be forwarded by name: %v", argv)
			assert.Equal(t, "-e", argv[keyIndex-1])
			assert.Equal(t, 1, strings.Count(string(argsData), key))
			assert.NotContains(t, string(argsData), secret)
			canonicalAgent, err := sandbox.CanonicalPath(agentPath)
			require.NoError(t, err)
			assert.Contains(t, argv, canonicalAgent, "agent paths must remain a single argument")
			envData, err := os.ReadFile(envFile)
			require.NoError(t, err)
			assert.Equal(t, secret, string(envData))
		})
	}
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
